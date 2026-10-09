package api

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"route/internal/claims"
	"route/internal/index"
	"route/internal/surge"
)

// writeJSON is the shared response helper for the read-only GET endpoints
// below.
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// clampInt parses a query param as an int, falling back to def and clamping
// to [0, max].
func clampInt(r *http.Request, param string, def, max int) int {
	v := r.URL.Query().Get(param)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// NewOrderGetHandler serves GET /orders/{id}.
func NewOrderGetHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		id := r.PathValue("id")

		var o struct {
			ID         string     `json:"id"`
			Status     string     `json:"status"`
			Priority   int        `json:"priority"`
			Attempts   int        `json:"attempts"`
			PickupLat  float64    `json:"pickup_lat"`
			PickupLng  float64    `json:"pickup_lng"`
			DropoffLat float64    `json:"dropoff_lat"`
			DropoffLng float64    `json:"dropoff_lng"`
			CreatedAt  time.Time  `json:"created_at"`
			UpdatedAt  time.Time  `json:"updated_at"`
			AssignedAt *time.Time `json:"assigned_at,omitempty"`
		}
		err := pool.QueryRow(r.Context(), `
			SELECT id, status, priority, attempts,
			       ST_Y(pickup::geometry), ST_X(pickup::geometry),
			       ST_Y(dropoff::geometry), ST_X(dropoff::geometry),
			       created_at, updated_at, assigned_at
			FROM orders WHERE id = $1
		`, id).Scan(&o.ID, &o.Status, &o.Priority, &o.Attempts,
			&o.PickupLat, &o.PickupLng, &o.DropoffLat, &o.DropoffLng,
			&o.CreatedAt, &o.UpdatedAt, &o.AssignedAt)
		if err != nil {
			if err == pgx.ErrNoRows {
				http.Error(w, "order not found", http.StatusNotFound)
				return
			}
			log.Printf("GET /orders/%s: %v", id, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, o)
	}
}

// NewCouriersHandler serves GET /couriers?status=available|claimed&limit=&offset=.
// Redis (not Postgres) is the source of truth for live courier state, so
// this reads the same GEO set the matcher does rather than a separate
// denormalized table.
func NewCouriersHandler(store *index.Store, claimsMgr *claims.Manager) http.HandlerFunc {
	type courierView struct {
		index.CourierState
		Status string `json:"status"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		statusFilter := r.URL.Query().Get("status")
		limit := clampInt(r, "limit", 100, 500)
		offset := clampInt(r, "offset", 0, 1<<30)

		all, err := store.GetAllActiveCouriers(r.Context())
		if err != nil {
			log.Printf("GET /couriers: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

		claimed := claimsMgr.BulkCheck(r.Context(), courierIDs(all))

		views := make([]courierView, 0, len(all))
		for _, c := range all {
			status := "available"
			if claimed[c.ID] {
				status = "claimed"
			}
			if statusFilter != "" && statusFilter != status {
				continue
			}
			views = append(views, courierView{CourierState: c, Status: status})
		}

		total := len(views)
		if offset > total {
			offset = total
		}
		end := offset + limit
		if end > total {
			end = total
		}

		writeJSON(w, map[string]interface{}{
			"couriers": views[offset:end],
			"total":    total,
		})
	}
}

func courierIDs(states []index.CourierState) []string {
	ids := make([]string, len(states))
	for i, s := range states {
		ids[i] = s.ID
	}
	return ids
}

// NewSurgeHandler serves GET /surge: the current multiplier for every zone
// with either demand or supply, per ROUTE_SPEC.md section 8.2.
func NewSurgeHandler(s *surge.Engine) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, s.GetState())
	}
}

// NewBatchesHandler serves GET /batches?since=<RFC3339>, recent solved
// batches with solver stats, per ROUTE_SPEC.md section 8.2.
func NewBatchesHandler(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		since := time.Now().Add(-1 * time.Hour)
		if v := r.URL.Query().Get("since"); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				http.Error(w, "since must be RFC3339", http.StatusBadRequest)
				return
			}
			since = t
		}
		limit := clampInt(r, "limit", 100, 500)

		rows, err := pool.Query(r.Context(), `
			SELECT id, window_open_at, window_close_at, order_count, courier_count,
			       solver, total_cost_batched, total_cost_greedy,
			       COALESCE(total_distance_batched, 0), COALESCE(total_distance_greedy, 0), solve_duration_ms
			FROM batches
			WHERE window_close_at >= $1
			ORDER BY window_close_at DESC
			LIMIT $2
		`, since, limit)
		if err != nil {
			log.Printf("GET /batches: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer rows.Close()

		type batchView struct {
			ID              string    `json:"id"`
			WindowOpenAt    time.Time `json:"window_open_at"`
			WindowCloseAt   time.Time `json:"window_close_at"`
			OrderCount      int       `json:"order_count"`
			CourierCount    int       `json:"courier_count"`
			Solver          string    `json:"solver"`
			TotalCostBatch  float64   `json:"total_cost_batched"`
			TotalCostGreedy float64   `json:"total_cost_greedy"`
			DistBatched     float64   `json:"total_distance_batched"`
			DistGreedy      float64   `json:"total_distance_greedy"`
			SolveDurationMs int       `json:"solve_duration_ms"`
		}
		batches := make([]batchView, 0, limit)
		for rows.Next() {
			var b batchView
			if err := rows.Scan(&b.ID, &b.WindowOpenAt, &b.WindowCloseAt, &b.OrderCount, &b.CourierCount,
				&b.Solver, &b.TotalCostBatch, &b.TotalCostGreedy, &b.DistBatched, &b.DistGreedy, &b.SolveDurationMs); err == nil {
				batches = append(batches, b)
			}
		}
		writeJSON(w, batches)
	}
}
