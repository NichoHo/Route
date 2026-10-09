package ingest

import (
	"context"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"route/internal/api/proto"
	"route/internal/auth"
	"route/internal/metrics"
	"route/internal/offererr"
)

type PositionStore interface {
	UpdatePosition(ctx context.Context, courierID string, lat, lng, heading float64) error
}

type PingWriter interface {
	Write(ping *proto.Ping)
}

type OfferResolver interface {
	Resolve(ctx context.Context, courierID, offerID string, accept bool) error
	Complete(ctx context.Context, orderID, courierID string)
}

type Handler struct {
	proto.UnimplementedCourierServer

	store    PositionStore
	history  PingWriter
	registry *StreamRegistry
	offers   OfferResolver
	auth     *auth.Auth

	maxSessions int64
	sessions    atomic.Int64
}

// NewHandler builds the courier gRPC handler. Sessions must present a valid
// per-courier token, and at most maxSessions may be open at once.
func NewHandler(store PositionStore, history PingWriter, registry *StreamRegistry, offers OfferResolver, a *auth.Auth, maxSessions int) *Handler {
	return &Handler{
		store:       store,
		history:     history,
		registry:    registry,
		offers:      offers,
		auth:        a,
		maxSessions: int64(maxSessions),
	}
}

// reasonFor turns an internal error into a small, client-safe reason string.
// Anything that isn't the well-known "not resolvable" case is collapsed to a
// generic reason; the real error is still logged server-side.
func reasonFor(err error) string {
	if errors.Is(err, offererr.ErrNotResolvable) {
		return "invalid_offer"
	}
	return "internal_error"
}

func (h *Handler) Session(stream proto.Courier_SessionServer) error {
	// Identity comes from the session's token, never from message contents.
	// The error is deliberately the same for every failure.
	courierID, token, ok := auth.CourierCredentials(stream.Context())
	if !ok || !h.auth.VerifyCourier(courierID, token) {
		return status.Error(codes.Unauthenticated, "invalid courier credentials")
	}
	if h.sessions.Add(1) > h.maxSessions {
		h.sessions.Add(-1)
		return status.Error(codes.ResourceExhausted, "too many courier sessions")
	}
	defer h.sessions.Add(-1)

	// A buffered channel of size 1 for backpressure. Each ping carries its
	// arrival time so the worker can report how long indexing lagged.
	type pingJob struct {
		ping   *proto.Ping
		recvAt time.Time
	}
	pingCh := make(chan pingJob, 1)
	defer close(pingCh)

	sw := h.registry.Register(courierID, stream)
	defer h.registry.Unregister(courierID, sw)

	// Background worker for this specific stream
	go func() {
		lastSeq := int64(-1)
		for job := range pingCh {
			ping := job.ping
			if ping.Seq <= lastSeq {
				metrics.OutOfOrderPingsTotal.Inc()
				continue
			}
			lastSeq = ping.Seq

			// Update Redis
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := h.store.UpdatePosition(ctx, ping.CourierId, ping.Lat, ping.Lng, ping.Heading)
			cancel()
			metrics.PingIndexLagSeconds.Observe(time.Since(job.recvAt).Seconds())

			if err != nil {
				log.Printf("redis update failed for %q: %v", ping.CourierId, err)
			}

			// Async to PostGIS
			h.history.Write(ping)
		}
	}()

	for {
		msg, err := stream.Recv()
		if err != nil {
			return err
		}
		recvAt := time.Now()

		if ping := msg.GetPing(); ping != nil {
			// A session may only report positions for the courier its token
			// names.
			if ping.CourierId != courierID {
				metrics.PingsRejectedTotal.WithLabelValues(reasonMismatch).Inc()
				continue
			}
			if reason := validatePing(ping, recvAt); reason != "" {
				metrics.PingsRejectedTotal.WithLabelValues(reason).Inc()
				continue
			}
			select {
			case pingCh <- pingJob{ping, recvAt}:
			default:
				// Channel is full. Replace the oldest ping with the newest one.
				select {
				case <-pingCh: // pop
				default:
				}
				pingCh <- pingJob{ping, recvAt} // push
				metrics.PingsShedTotal.Inc()
			}

			// Send Ack immediately
			err = sw.Send(&proto.ServerMessage{
				Payload: &proto.ServerMessage_Ack{
					Ack: &proto.Ack{Seq: ping.Seq},
				},
			})
			if err != nil {
				log.Printf("failed to send ack: %v", err)
				return err
			}
		} else if resp := msg.GetOfferResponse(); resp != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			err := h.offers.Resolve(ctx, courierID, resp.OfferId, resp.Accept)
			cancel()

			outcome := &proto.OfferOutcome{
				OfferId: resp.OfferId,
				Success: err == nil,
			}
			if err != nil {
				outcome.Reason = reasonFor(err)
				log.Printf("failed to resolve offer %q: %v", resp.OfferId, err)
			}

			_ = sw.Send(&proto.ServerMessage{
				Payload: &proto.ServerMessage_OfferOutcome{
					OfferOutcome: outcome,
				},
			})
		} else if report := msg.GetCompletionReport(); report != nil {
			h.offers.Complete(context.Background(), report.OrderId, courierID)
		}
	}
}
