package offers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"route/internal/api/proto"
	"route/internal/claims"
	"route/internal/config"
	"route/internal/ingest"
	"route/internal/metrics"
	"route/internal/offererr"

	"go.opentelemetry.io/otel"
)

// ErrOrderNotCancellable is returned when an order_id doesn't name an order
// in a pre-assigned state (queued/batched/offered). Safe to expose to
// clients.
var ErrOrderNotCancellable = errors.New("order not found or already assigned/terminal")

// ErrOrderNotBatched is returned by CreateOffer when the order is no longer in
// the 'batched' state (cancelled or exhausted while its batch was solving).
var ErrOrderNotBatched = errors.New("order is no longer batched")

// ErrCourierBusy is returned by CreateOffer when Postgres says the courier is
// already committed (an active assignment or a pending offer) even though
// Redis let the claim through. That means Redis lost the courier's claim.
var ErrCourierBusy = errors.New("courier already holds an offer or assignment")

// OutboxWriter is a narrow interface over events.Outbox so that Lifecycle can
// be tested without a live Kafka/Redpanda connection. *events.Outbox satisfies
// this interface automatically.
type OutboxWriter interface {
	Insert(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error
	Publish(ctx context.Context, topic string, payload []byte) error
}

type Lifecycle struct {
	pool    *pgxpool.Pool
	claims  *claims.Manager
	streams *ingest.StreamRegistry
	matcher MatchTrigger
	outbox  OutboxWriter

	reconcileHook func() // test seam, see SetReconcileHook
}

type MatchTrigger interface {
	Trigger(orderID string)
}

func NewLifecycle(pool *pgxpool.Pool, claims *claims.Manager, streams *ingest.StreamRegistry, matcher MatchTrigger, outbox OutboxWriter) *Lifecycle {
	return &Lifecycle{
		pool:    pool,
		claims:  claims,
		streams: streams,
		matcher: matcher,
		outbox:  outbox,
	}
}

// CreateOffer records an offer to courierID for orderID. cost is the matcher's
// objective cost; pickupM is the courier-to-pickup distance in meters.
func (l *Lifecycle) CreateOffer(ctx context.Context, offerID, orderID, courierID string, cost, pickupM float64) error {
	ctx, span := otel.Tracer("offers").Start(ctx, "CreateOffer")
	defer span.End()

	// Fail fast if the courier has no live stream to receive the offer on,
	// instead of writing a pending offer that can only ever time out.
	if !l.streams.HasStream(courierID) {
		return offererr.ErrNotResolvable
	}

	deadline := time.Now().Add(config.OfferDeadline)

	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Postgres is the durable record: refuse a courier that already holds an
	// assignment, and (via offers_pending_courier_idx) one that already holds
	// a pending offer, whatever Redis says. Redis can lose claims.
	tag, err := tx.Exec(ctx, `
		INSERT INTO offers (id, order_id, courier_id, state, cost, pickup_m, deadline_at)
		SELECT $1::text, $2::text, $3::text, 'pending', $4::numeric, $6::numeric, $5::timestamptz
		WHERE NOT EXISTS (SELECT 1 FROM assignments WHERE courier_id = $3::text AND completed_at IS NULL)`,
		offerID, orderID, courierID, cost, deadline, pickupM)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "offers_pending_courier_idx" {
			return ErrCourierBusy
		}
		span.RecordError(err)
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrCourierBusy
	}

	// Guarded: if the order was cancelled or exhausted while this batch was
	// solving, don't resurrect it. The offer insert rolls back with the tx.
	tag, err = tx.Exec(ctx, `UPDATE orders SET status = 'offered', updated_at = NOW() WHERE id = $1 AND status = 'batched'`, orderID)
	if err != nil {
		span.RecordError(err)
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrOrderNotBatched
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"offer_id":   offerID,
		"order_id":   orderID,
		"courier_id": courierID,
		"deadline":   deadline,
	})
	l.outbox.Insert(ctx, tx, "offer.sent", payload)

	if err := tx.Commit(ctx); err != nil {
		span.RecordError(err)
		return err
	}

	// Send via stream. A failure here (the courier disconnected between the
	// HasStream check and now) is rare; the offer sweep will expire it like
	// any other unanswered offer.
	if err := l.streams.SendOffer(courierID, &proto.Offer{
		OfferId:    offerID,
		OrderId:    orderID,
		DeadlineTs: deadline.UnixMilli(),
	}); err != nil {
		log.Printf("failed to send offer %s to %s: %v", offerID, courierID, err)
	}

	return nil
}

// resolveFailedOffer moves a pending offer to a terminal non-accepted state
// (declined or expired), bumps the order's attempt count, and either
// re-queues it or exhausts it. courierID is the identity claimed by the
// caller; empty means "no identity to check" and is only used by the
// server-driven sweep (ExpireOffer), never by client input.
func (l *Lifecycle) resolveFailedOffer(ctx context.Context, offerID, courierID, reason string) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var orderID, actualCourierID string
	if courierID != "" {
		err = tx.QueryRow(ctx, `UPDATE offers SET state = $1, resolved_at = NOW() WHERE id = $2 AND courier_id = $3 AND state = 'pending' RETURNING order_id, courier_id`, reason, offerID, courierID).Scan(&orderID, &actualCourierID)
	} else {
		err = tx.QueryRow(ctx, `UPDATE offers SET state = $1, resolved_at = NOW() WHERE id = $2 AND state = 'pending' RETURNING order_id, courier_id`, reason, offerID).Scan(&orderID, &actualCourierID)
	}
	if err != nil {
		return offererr.ErrNotResolvable
	}

	var attempts int
	var status string
	err = tx.QueryRow(ctx, `UPDATE orders SET attempts = attempts + 1, excluded_couriers = array_append(excluded_couriers, $1), status = CASE WHEN attempts + 1 >= $3 THEN 'exhausted' ELSE 'queued' END, priority = LEAST(priority + 1, $4), updated_at = NOW() WHERE id = $2 AND status = 'offered' RETURNING attempts, status`, actualCourierID, orderID, config.MaxAttempts, config.MaxPriority).Scan(&attempts, &status)
	if err != nil {
		return err
	}

	payload, _ := json.Marshal(map[string]interface{}{"offer_id": offerID, "outcome": reason})
	l.outbox.Insert(ctx, tx, "offer.resolved", payload)

	if status == "exhausted" {
		payload2, _ := json.Marshal(map[string]interface{}{"order_id": orderID, "attempts": attempts})
		l.outbox.Insert(ctx, tx, "order.exhausted", payload2)
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	metrics.OfferOutcomeTotal.WithLabelValues(reason).Inc()

	l.claims.ReleaseClaim(ctx, actualCourierID, offerID)
	if attempts < config.MaxAttempts {
		l.matcher.Trigger(orderID)
	}
	return nil
}

func (l *Lifecycle) ExpireOffer(ctx context.Context, offerID string) {
	_ = l.resolveFailedOffer(ctx, offerID, "", "expired")
}

func (l *Lifecycle) Resolve(ctx context.Context, courierID, offerID string, accept bool) error {
	if accept {
		return l.acceptOffer(ctx, courierID, offerID)
	}
	return l.resolveFailedOffer(ctx, offerID, courierID, "declined")
}

func (l *Lifecycle) acceptOffer(ctx context.Context, courierID, offerID string) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var orderID, actualCourierID string
	var cost float64
	err = tx.QueryRow(ctx, `UPDATE offers SET state = 'accepted', resolved_at = NOW() WHERE id = $1 AND courier_id = $2 AND state = 'pending' RETURNING order_id, courier_id, cost`, offerID, courierID).Scan(&orderID, &actualCourierID, &cost)
	if err != nil {
		return offererr.ErrNotResolvable
	}

	assignmentID := uuid.New().String()

	_, err = tx.Exec(ctx, `INSERT INTO assignments (id, order_id, courier_id, offer_id) VALUES ($1, $2, $3, $4)`, assignmentID, orderID, courierID, offerID)
	if err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `UPDATE orders SET status = 'assigned', assigned_at = NOW(), updated_at = NOW() WHERE id = $1 AND status = 'offered'`, orderID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return offererr.ErrNotResolvable
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"order_id":   orderID,
		"courier_id": courierID,
		"cost":       cost,
	})
	if err := l.outbox.Insert(ctx, tx, "order.assigned", payload); err != nil {
		return err
	}

	payload2, _ := json.Marshal(map[string]interface{}{
		"offer_id": offerID,
		"outcome":  "accepted",
	})
	if err := l.outbox.Insert(ctx, tx, "offer.resolved", payload2); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	// The assignment is committed; now point the courier's claim at it. This
	// goes after the commit, never before: a claim converted for a
	// transaction that then rolls back (or whose reply is lost) would name an
	// assignment that does not exist and lock the courier for
	// AssignmentClaimTTL. If this fails, the committed assignment stands, the
	// offer claim still covers the courier, and ReconcileClaims repairs it.
	if _, err := l.claims.Restore(ctx, courierID, assignmentID, offerID, config.AssignmentClaimTTL); err != nil {
		log.Printf("accept %q: claim not converted, reconciliation will repair it: %v", offerID, err)
	}

	metrics.OfferOutcomeTotal.WithLabelValues("accepted").Inc()

	return nil
}

// CancelOrder cancels an order, but only from a pre-assigned state
// (queued/batched/offered), per the order state machine in
// ROUTE_SPEC.md section 5.3. If the order had a pending offer, that offer
// is expired and its courier claim released so the courier isn't left
// claimed for an order that no longer exists.
func (l *Lifecycle) CancelOrder(ctx context.Context, orderID string) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `
		UPDATE orders SET status = 'cancelled', cancelled_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status IN ('queued', 'batched', 'offered')
		RETURNING status
	`, orderID).Scan(&status)
	if err != nil {
		return ErrOrderNotCancellable
	}

	var offerID, courierID string
	hasOffer := true
	if err := tx.QueryRow(ctx, `
		UPDATE offers SET state = 'expired', resolved_at = NOW()
		WHERE order_id = $1 AND state = 'pending'
		RETURNING id, courier_id
	`, orderID).Scan(&offerID, &courierID); err != nil {
		hasOffer = false
	}

	if err := tx.Commit(ctx); err != nil {
		return err
	}

	if hasOffer {
		l.claims.ReleaseClaim(ctx, courierID, offerID)
	}
	return nil
}

func (l *Lifecycle) Complete(ctx context.Context, orderID, courierID string) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)

	var assignmentID, offerID string
	err = tx.QueryRow(ctx, `UPDATE assignments SET completed_at = NOW() WHERE order_id = $1 AND courier_id = $2 AND completed_at IS NULL RETURNING id, offer_id`, orderID, courierID).Scan(&assignmentID, &offerID)
	if err != nil {
		// Already completed, or this courier doesn't hold this order's
		// assignment. Either way, nothing to do.
		return
	}

	if _, err := tx.Exec(ctx, `UPDATE orders SET status = 'completed', completed_at = NOW(), updated_at = NOW() WHERE id = $1`, orderID); err != nil {
		return
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"order_id":   orderID,
		"courier_id": courierID,
	})
	if err := l.outbox.Insert(ctx, tx, "order.completed", payload); err != nil {
		return
	}

	if err := tx.Commit(ctx); err != nil {
		return
	}

	// The claim is normally the assignment's; it is still the offer's if the
	// conversion on accept failed and reconciliation has not run yet.
	l.claims.ReleaseClaim(ctx, courierID, assignmentID)
	l.claims.ReleaseClaim(ctx, courierID, offerID)
}

// StartSweep runs the background sweep that keeps the system moving forward
// without relying on any in-process timer surviving a restart: it expires
// timed-out offers, re-triggers orders stuck in queued/batched, and gives up
// on orders that have gone unmatched for too long.
func (l *Lifecycle) StartSweep(ctx context.Context) {
	// Reconcile before the first sweep: after a restart, or when Redis came
	// back empty, claims must be re-established before a batch can offer a
	// courier that Postgres knows is busy.
	l.ReconcileClaims(ctx)

	ticker := time.NewTicker(config.SweepInterval)
	reconcile := time.NewTicker(config.ReconcileInterval)
	go func() {
		defer ticker.Stop()
		defer reconcile.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				l.sweep(context.Background())
			case <-reconcile.C:
				l.ReconcileClaims(ctx)
			}
		}
	}()
}

// ReconcileClaims checks every courier claim Postgres says must exist (one
// per pending offer and one per active assignment) against Redis and repairs
// what it can: a missing claim is re-created, and an offer claim left on a
// courier whose offer was accepted becomes the assignment claim. A claim held
// under some other value is counted and logged, not overwritten.
func (l *Lifecycle) ReconcileClaims(ctx context.Context) {
	l.reconcile(ctx, "")
}

// SetReconcileHook sets a function that runs after reconciliation has read
// what Postgres says and before it touches Redis. It exists for tests that
// need an offer or assignment to finish in exactly that gap.
func (l *Lifecycle) SetReconcileHook(f func()) { l.reconcileHook = f }

// stillLive reports whether the offer is still pending, or the assignment
// still active.
func (l *Lifecycle) stillLive(ctx context.Context, kind, id string) bool {
	query := `SELECT state = 'pending' FROM offers WHERE id = $1`
	if kind == "assignment" {
		query = `SELECT completed_at IS NULL FROM assignments WHERE id = $1`
	}
	var live bool
	if err := l.pool.QueryRow(ctx, query, id).Scan(&live); err != nil {
		return false
	}
	return live
}

// RepairCourierClaim reconciles one courier's claim.
func (l *Lifecycle) RepairCourierClaim(ctx context.Context, courierID string) {
	l.reconcile(ctx, courierID)
}

func (l *Lifecycle) reconcile(ctx context.Context, courierID string) {
	type want struct {
		courierID, value, replaceable string
		kind                          string // "offer" or "assignment"
		ttl                           time.Duration
	}
	var wants []want

	rows, err := l.pool.Query(ctx, `SELECT id, courier_id, deadline_at FROM offers
		WHERE state = 'pending' AND ($1 = '' OR courier_id = $1)`, courierID)
	if err != nil {
		log.Printf("reconcile: pending offers: %v", err)
		return
	}
	for rows.Next() {
		var id, courier string
		var deadline time.Time
		if err := rows.Scan(&id, &courier, &deadline); err == nil {
			remaining := time.Until(deadline)
			if remaining < 0 {
				remaining = 0
			}
			// Same grace the matcher gives a fresh offer claim.
			wants = append(wants, want{courier, id, "", "offer", remaining + (config.OfferClaimTTL - config.OfferDeadline)})
		}
	}
	rows.Close()

	rows, err = l.pool.Query(ctx, `SELECT id, courier_id, offer_id FROM assignments
		WHERE completed_at IS NULL AND ($1 = '' OR courier_id = $1)`, courierID)
	if err != nil {
		log.Printf("reconcile: active assignments: %v", err)
		return
	}
	for rows.Next() {
		var id, courier, offerID string
		if err := rows.Scan(&id, &courier, &offerID); err == nil {
			wants = append(wants, want{courier, id, offerID, "assignment", config.AssignmentClaimTTL})
		}
	}
	rows.Close()

	if l.reconcileHook != nil {
		l.reconcileHook()
	}

	var restored, mismatched int
	for _, w := range wants {
		res, err := l.claims.Restore(ctx, w.courierID, w.value, w.replaceable, w.ttl)
		switch {
		case err != nil:
			log.Printf("reconcile: courier %q: %v", w.courierID, err)
		case res == claims.ClaimRestored:
			// The offer or assignment was live when it was read, but it can
			// have been resolved or completed since, and its release can have
			// run before this restore. Check again, and undo the restore if so.
			// Without this, a finished assignment gets a claim that lasts
			// AssignmentClaimTTL and its courier sits out for an hour. Any
			// release after this check deletes the claim as usual.
			if !l.stillLive(ctx, w.kind, w.value) {
				l.claims.ReleaseClaim(ctx, w.courierID, w.value)
				continue
			}
			restored++
		case res == claims.ClaimMismatch:
			mismatched++
		}
	}
	if restored > 0 {
		metrics.ClaimsReconciledTotal.WithLabelValues("restored").Add(float64(restored))
	}
	if mismatched > 0 {
		metrics.ClaimsReconciledTotal.WithLabelValues("mismatch").Add(float64(mismatched))
	}
	if restored+mismatched > 0 && courierID == "" {
		log.Printf("reconcile: restored %d claims, %d held under another value", restored, mismatched)
	}
}

func (l *Lifecycle) sweep(ctx context.Context) {
	l.expireStaleOffers(ctx)
	l.requeueStuckOrders(ctx)
	l.exhaustStaleOrders(ctx)
}

func (l *Lifecycle) expireStaleOffers(ctx context.Context) {
	rows, err := l.pool.Query(ctx, `SELECT id FROM offers WHERE state = 'pending' AND deadline_at < NOW()`)
	if err != nil {
		log.Printf("sweep: failed to query expired offers: %v", err)
		return
	}
	var expiredIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			expiredIDs = append(expiredIDs, id)
		}
	}
	rows.Close()
	for _, id := range expiredIDs {
		l.ExpireOffer(ctx, id)
	}
}

func (l *Lifecycle) requeueStuckOrders(ctx context.Context) {
	// One round trip: put stuck orders back to 'queued' (SolveBatch only
	// picks up 'queued', so an order stranded in 'batched' by a crash or an
	// empty courier pool would never be solved again otherwise) and return
	// the ids in the same statement.
	// Oldest first, and at most 1000 per pass. Under overload there can be
	// tens of thousands of stuck orders; waking them all at once only floods a
	// queue that is already full.
	rows, err := l.pool.Query(ctx, `
		UPDATE orders SET status = 'queued', updated_at = NOW()
		WHERE id IN (
			SELECT id FROM orders
			WHERE status IN ('queued', 'batched') AND updated_at < NOW() - ($1 * INTERVAL '1 second')
			ORDER BY updated_at
			LIMIT 1000
		)
		RETURNING id
	`, config.RequeueStaleAfter.Seconds())
	if err != nil {
		log.Printf("sweep: failed to requeue stuck orders: %v", err)
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		l.matcher.Trigger(id)
	}
}

// exhaustStaleOrders gives up on orders that have been unmatched for too
// long (no courier ever came within the search radius). Without this, an
// order with no reachable courier would sit in queued/batched forever,
// endlessly re-triggered by requeueStuckOrders.
func (l *Lifecycle) exhaustStaleOrders(ctx context.Context) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return
	}
	defer tx.Rollback(ctx)

	rows, err := tx.Query(ctx, `
		UPDATE orders SET status = 'exhausted', updated_at = NOW()
		WHERE status IN ('queued', 'batched') AND created_at < NOW() - ($1 * INTERVAL '1 second')
		RETURNING id, attempts
	`, config.MaxQueueAge.Seconds())
	if err != nil {
		log.Printf("sweep: failed to exhaust stale orders: %v", err)
		return
	}
	type stale struct {
		id       string
		attempts int
	}
	var exhausted []stale
	for rows.Next() {
		var s stale
		if err := rows.Scan(&s.id, &s.attempts); err == nil {
			exhausted = append(exhausted, s)
		}
	}
	rows.Close()

	for _, s := range exhausted {
		payload, _ := json.Marshal(map[string]interface{}{"order_id": s.id, "attempts": s.attempts})
		l.outbox.Insert(ctx, tx, "order.exhausted", payload)
	}

	if err := tx.Commit(ctx); err != nil {
		log.Printf("sweep: failed to commit stale-order exhaustion: %v", err)
	}
}
