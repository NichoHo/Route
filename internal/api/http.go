package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber/h3-go/v4"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"route/internal/config"
	"route/internal/matcher"
	"route/internal/offers"
	"route/internal/surge"
)

// maxBodyBytes caps the request body so a client can't tie up a handler (or
// disk, via the outbox payload) with an arbitrarily large POST /orders.
const maxBodyBytes = 4096

// maxIdempotencyKeyLen bounds what gets stored and indexed as a unique key.
const maxIdempotencyKeyLen = 200

// minPriority/maxPriority bound the documented priority field.
const (
	minPriority = 0
	maxPriority = config.MaxPriority
)

type CreateOrderRequest struct {
	PickupLat  float64 `json:"pickup_lat"`
	PickupLng  float64 `json:"pickup_lng"`
	DropoffLat float64 `json:"dropoff_lat"`
	DropoffLng float64 `json:"dropoff_lng"`
	Priority   int     `json:"priority"`
}

// outboxInserter is the part of events.Outbox the order API needs: writing
// an event inside the order's transaction.
type outboxInserter interface {
	Insert(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error
}

type OrderAPI struct {
	pool    *pgxpool.Pool
	batcher *matcher.Batcher
	outbox  outboxInserter
	surge   *surge.Engine
}

func NewOrderAPI(pool *pgxpool.Pool, b *matcher.Batcher, o outboxInserter, s *surge.Engine) *OrderAPI {
	return &OrderAPI{pool: pool, batcher: b, outbox: o, surge: s}
}

func inJakartaBBox(lat, lng float64) bool {
	return lat >= config.JakartaMinLat && lat <= config.JakartaMaxLat &&
		lng >= config.JakartaMinLng && lng <= config.JakartaMaxLng
}

func (req CreateOrderRequest) validate() string {
	if !inJakartaBBox(req.PickupLat, req.PickupLng) {
		return "pickup coordinates are outside the service area"
	}
	if !inJakartaBBox(req.DropoffLat, req.DropoffLng) {
		return "dropoff coordinates are outside the service area"
	}
	if req.Priority < minPriority || req.Priority > maxPriority {
		return "priority out of range"
	}
	return ""
}

func (api *OrderAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idempotencyKey := r.Header.Get("Idempotency-Key")
	if idempotencyKey == "" {
		http.Error(w, "Idempotency-Key header is required", http.StatusBadRequest)
		return
	}
	if len(idempotencyKey) > maxIdempotencyKeyLen {
		http.Error(w, "Idempotency-Key is too long", http.StatusBadRequest)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields() // reject stray fields (status, assigned_courier, ...)

	var req CreateOrderRequest
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	if reason := req.validate(); reason != "" {
		http.Error(w, reason, http.StatusBadRequest)
		return
	}

	tracer := otel.Tracer("http")
	ctx, span := tracer.Start(r.Context(), "POST /orders")
	defer span.End()

	orderID := uuid.New().String()
	span.SetAttributes(attribute.String("order.id", orderID))

	cell, err := h3.LatLngToCell(h3.NewLatLng(req.PickupLat, req.PickupLng), 8)
	if err != nil {
		http.Error(w, "invalid coordinates", http.StatusBadRequest)
		span.RecordError(err)
		return
	}

	tx, err := api.pool.Begin(ctx)
	if err != nil {
		log.Printf("POST /orders: begin tx: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		span.RecordError(err)
		return
	}
	defer tx.Rollback(ctx)

	query := `
		INSERT INTO orders (id, idempotency_key, pickup, dropoff, pickup_h3, priority, status)
		VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326), ST_SetSRID(ST_MakePoint($5, $6), 4326), $7, $8, 'queued')
		ON CONFLICT (idempotency_key) DO NOTHING
		RETURNING id
	`
	err = tx.QueryRow(ctx, query, orderID, idempotencyKey, req.PickupLng, req.PickupLat, req.DropoffLng, req.DropoffLat, cell.String(), req.Priority).Scan(&orderID)

	if err != nil {
		if err != pgx.ErrNoRows {
			log.Printf("POST /orders: insert: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			span.RecordError(err)
			return
		}
		// Idempotency conflict: a prior request with this key already
		// created an order. Return it unchanged, per the spec's contract
		// for a repeated Idempotency-Key.
		err = api.pool.QueryRow(ctx, `SELECT id FROM orders WHERE idempotency_key = $1`, idempotencyKey).Scan(&orderID)
		if err != nil {
			log.Printf("POST /orders: lookup existing: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			span.RecordError(err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": orderID, "status": "existing"})
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"id":        orderID,
		"pickup_h3": cell.String(),
		// Propagate trace context
		"traceparent": getTraceparent(ctx),
	})
	if err := api.outbox.Insert(ctx, tx, "order.created", payload); err != nil {
		log.Printf("POST /orders: outbox insert: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		span.RecordError(err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("POST /orders: commit: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		span.RecordError(err)
		return
	}

	api.surge.RecordOrder(req.PickupLat, req.PickupLng)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]string{"id": orderID, "status": "created"})

	api.batcher.Trigger(orderID)
}

// NewCancelHandler serves POST /orders/{id}/cancel.
func NewCancelHandler(lifecycle *offers.Lifecycle) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.PathValue("id")
		if err := lifecycle.CancelOrder(r.Context(), id); err != nil {
			if errors.Is(err, offers.ErrOrderNotCancellable) {
				http.Error(w, "order not found or already assigned/terminal", http.StatusConflict)
				return
			}
			log.Printf("POST /orders/%s/cancel: %v", id, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"id": id, "status": "cancelled"})
	}
}

func getTraceparent(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if sc.IsValid() {
		return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + sc.TraceFlags().String()
	}
	return ""
}
