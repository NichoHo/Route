package offers_test

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"google.golang.org/grpc/metadata"
	"route/internal/api/proto"
	"route/internal/claims"
	"route/internal/config"
	"route/internal/ingest"
	"route/internal/offers"
)

type MockMatcher struct {
	triggerCount int
}

func (m *MockMatcher) Trigger(orderID string) {
	m.triggerCount++
}

// mockOutbox is a no-op OutboxWriter for unit/integration tests that do not
// need event publishing.
type mockOutbox struct{}

func (m *mockOutbox) Insert(_ context.Context, _ pgx.Tx, _ string, _ []byte) error { return nil }
func (m *mockOutbox) Publish(_ context.Context, _ string, _ []byte) error          { return nil }

type mockCourierStream struct {
	ctx context.Context
}

func (m *mockCourierStream) Send(msg *proto.ServerMessage) error  { return nil }
func (m *mockCourierStream) Recv() (*proto.CourierMessage, error) { return nil, nil }
func (m *mockCourierStream) SetHeader(metadata.MD) error          { return nil }
func (m *mockCourierStream) SendHeader(metadata.MD) error         { return nil }
func (m *mockCourierStream) SetTrailer(metadata.MD)               {}
func (m *mockCourierStream) Context() context.Context             { return m.ctx }
func (m *mockCourierStream) SendMsg(m_ interface{}) error         { return nil }
func (m *mockCourierStream) RecvMsg(m_ interface{}) error         { return nil }

func TestIdempotentAccept(t *testing.T) {
	pgUrl := os.Getenv("POSTGRES_URL")
	if pgUrl == "" {
		pgUrl = "postgres://route:routepassword@localhost:5432/route?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), pgUrl)
	if err != nil {
		t.Fatalf("db connect failed: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		t.Skip("Postgres not running, skipping test")
	}

	redisUrl := os.Getenv("REDIS_ADDR")
	if redisUrl == "" {
		redisUrl = "localhost:6379"
	}
	redisClient := redis.NewClient(&redis.Options{Addr: redisUrl})
	defer redisClient.Close()
	claimsMgr := claims.NewManager(redisClient)

	streams := ingest.NewStreamRegistry()
	mockMatcher := &MockMatcher{}
	lifecycle := offers.NewLifecycle(pool, claimsMgr, streams, mockMatcher, &mockOutbox{})

	// Setup data
	pool.Exec(context.Background(), "TRUNCATE orders, assignments, offers CASCADE")

	orderID := uuid.New().String()
	courierID := uuid.New().String()
	offerID := uuid.New().String()

	streams.Register(courierID, &mockCourierStream{ctx: context.Background()})

	_, err = pool.Exec(context.Background(), `INSERT INTO orders (id, idempotency_key, pickup, dropoff, pickup_h3, status) VALUES ($1, $2, ST_SetSRID(ST_MakePoint(0,0), 4326), ST_SetSRID(ST_MakePoint(1,1), 4326), 'h3', 'batched')`, orderID, uuid.New().String())
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}

	claimsMgr.AcquireOfferClaim(context.Background(), courierID, offerID, 15*time.Second)

	err = lifecycle.CreateOffer(context.Background(), offerID, orderID, courierID, 10.0, 0)
	if err != nil {
		t.Fatalf("failed to create offer: %v", err)
	}

	// Concurrent Accepts
	var successCount int
	var errCount int
	var errs []error
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := lifecycle.Resolve(context.Background(), courierID, offerID, true)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successCount++
			} else {
				errCount++
				errs = append(errs, err)
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 success, got %d, errors: %v", successCount, errs)
	}
	if errCount != 19 {
		t.Fatalf("expected 19 errors, got %d", errCount)
	}
}

type testEnv struct {
	pool      *pgxpool.Pool
	redis     *redis.Client
	claims    *claims.Manager
	streams   *ingest.StreamRegistry
	lifecycle *offers.Lifecycle
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	pgUrl := os.Getenv("POSTGRES_URL")
	if pgUrl == "" {
		pgUrl = "postgres://route:routepassword@localhost:5432/route?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), pgUrl)
	if err != nil {
		t.Fatalf("db connect failed: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(context.Background()); err != nil {
		t.Skip("Postgres not running, skipping test")
	}
	redisUrl := os.Getenv("REDIS_ADDR")
	if redisUrl == "" {
		redisUrl = "localhost:6379"
	}
	rc := redis.NewClient(&redis.Options{Addr: redisUrl})
	t.Cleanup(func() { rc.Close() })
	if err := rc.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not running, skipping test")
	}
	pool.Exec(context.Background(), "TRUNCATE orders, assignments, offers CASCADE")

	claimsMgr := claims.NewManager(rc)
	streams := ingest.NewStreamRegistry()
	return &testEnv{
		pool:      pool,
		redis:     rc,
		claims:    claimsMgr,
		streams:   streams,
		lifecycle: offers.NewLifecycle(pool, claimsMgr, streams, &MockMatcher{}, &mockOutbox{}),
	}
}

func (e *testEnv) insertOrder(t *testing.T, status string) string {
	t.Helper()
	id := uuid.New().String()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO orders (id, idempotency_key, pickup, dropoff, pickup_h3, status) VALUES ($1, $2, ST_SetSRID(ST_MakePoint(0,0), 4326), ST_SetSRID(ST_MakePoint(1,1), 4326), 'h3', $3)`, id, uuid.New().String(), status)
	if err != nil {
		t.Fatalf("failed to insert order: %v", err)
	}
	return id
}

func (e *testEnv) orderStatus(t *testing.T, id string) string {
	t.Helper()
	var status string
	if err := e.pool.QueryRow(context.Background(), `SELECT status FROM orders WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("failed to read order status: %v", err)
	}
	return status
}

// openOffer puts a fresh batched order into the 'offered' state with a held
// claim, the way the matcher does.
func (e *testEnv) openOffer(t *testing.T) (orderID, courierID, offerID string) {
	t.Helper()
	orderID = e.insertOrder(t, "batched")
	courierID = uuid.New().String()
	offerID = uuid.New().String()
	e.streams.Register(courierID, &mockCourierStream{ctx: context.Background()})
	if ok, err := e.claims.AcquireOfferClaim(context.Background(), courierID, offerID, 15*time.Second); err != nil || !ok {
		t.Fatalf("claim failed: %v %v", ok, err)
	}
	if err := e.lifecycle.CreateOffer(context.Background(), offerID, orderID, courierID, 1.0, 0); err != nil {
		t.Fatalf("CreateOffer failed: %v", err)
	}
	return
}

func (e *testEnv) claimHeld(courierID string) bool {
	n, _ := e.redis.Exists(context.Background(), "route:claim:"+courierID).Result()
	return n > 0
}

// A cancel that lands while the order's batch is solving must not be
// overwritten by the offer that batch then tries to create.
func TestCreateOfferRejectsCancelledOrder(t *testing.T) {
	e := newTestEnv(t)
	orderID := e.insertOrder(t, "cancelled")
	courierID := uuid.New().String()
	e.streams.Register(courierID, &mockCourierStream{ctx: context.Background()})

	err := e.lifecycle.CreateOffer(context.Background(), uuid.New().String(), orderID, courierID, 1.0, 0)
	if err != offers.ErrOrderNotBatched {
		t.Fatalf("expected ErrOrderNotBatched, got %v", err)
	}
	if got := e.orderStatus(t, orderID); got != "cancelled" {
		t.Fatalf("order status = %s, want cancelled", got)
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM offers WHERE order_id = $1`, orderID).Scan(&n)
	if n != 0 {
		t.Fatalf("offer row leaked for cancelled order")
	}
}

func TestLateAcceptRejectedAndClaimReleased(t *testing.T) {
	e := newTestEnv(t)
	orderID, courierID, offerID := e.openOffer(t)

	e.lifecycle.ExpireOffer(context.Background(), offerID)

	if err := e.lifecycle.Resolve(context.Background(), courierID, offerID, true); err == nil {
		t.Fatal("late accept should be rejected")
	}
	if got := e.orderStatus(t, orderID); got != "queued" {
		t.Fatalf("order status = %s, want queued after expiry", got)
	}
	if e.claimHeld(courierID) {
		t.Fatal("claim must be released after expiry")
	}
	var n int
	e.pool.QueryRow(context.Background(), `SELECT count(*) FROM assignments WHERE order_id = $1`, orderID).Scan(&n)
	if n != 0 {
		t.Fatal("late accept created an assignment")
	}
}

func TestDeclineReleasesClaimAndExcludesCourier(t *testing.T) {
	e := newTestEnv(t)
	orderID, courierID, offerID := e.openOffer(t)

	if err := e.lifecycle.Resolve(context.Background(), courierID, offerID, false); err != nil {
		t.Fatalf("decline failed: %v", err)
	}
	if e.claimHeld(courierID) {
		t.Fatal("claim must be released after decline")
	}
	var excluded []string
	var priority int
	e.pool.QueryRow(context.Background(), `SELECT excluded_couriers, priority FROM orders WHERE id = $1`, orderID).Scan(&excluded, &priority)
	if len(excluded) != 1 || excluded[0] != courierID {
		t.Fatalf("excluded_couriers = %v, want [%s]", excluded, courierID)
	}
	if priority != 1 {
		t.Fatalf("priority = %d, want 1 (boosted by the failed offer)", priority)
	}
}

func TestCancelReleasesClaim(t *testing.T) {
	e := newTestEnv(t)
	orderID, courierID, offerID := e.openOffer(t)

	if err := e.lifecycle.CancelOrder(context.Background(), orderID); err != nil {
		t.Fatalf("cancel failed: %v", err)
	}
	if e.claimHeld(courierID) {
		t.Fatal("claim must be released after cancel")
	}
	if err := e.lifecycle.Resolve(context.Background(), courierID, offerID, true); err == nil {
		t.Fatal("accept after cancel should be rejected")
	}
	if got := e.orderStatus(t, orderID); got != "cancelled" {
		t.Fatalf("order status = %s, want cancelled", got)
	}
}

func TestAttemptCapExhaustsOrder(t *testing.T) {
	e := newTestEnv(t)
	orderID := e.insertOrder(t, "batched")
	for i := 0; i < config.MaxAttempts; i++ {
		courierID := uuid.New().String()
		offerID := uuid.New().String()
		e.streams.Register(courierID, &mockCourierStream{ctx: context.Background()})
		e.claims.AcquireOfferClaim(context.Background(), courierID, offerID, 15*time.Second)
		if err := e.lifecycle.CreateOffer(context.Background(), offerID, orderID, courierID, 1.0, 0); err != nil {
			t.Fatalf("offer %d failed: %v", i, err)
		}
		if err := e.lifecycle.Resolve(context.Background(), courierID, offerID, false); err != nil {
			t.Fatalf("decline %d failed: %v", i, err)
		}
		if i < config.MaxAttempts-1 {
			// The matcher would move it queued -> batched before the next offer.
			e.pool.Exec(context.Background(), `UPDATE orders SET status = 'batched' WHERE id = $1`, orderID)
		}
	}
	if got := e.orderStatus(t, orderID); got != "exhausted" {
		t.Fatalf("order status = %s, want exhausted", got)
	}
}

// An order stranded in 'batched' (crash, or a batch that found no couriers)
// must go back to 'queued' so SolveBatch will pick it up again.
func TestSweepRequeuesStuckBatchedOrder(t *testing.T) {
	e := newTestEnv(t)
	orderID := e.insertOrder(t, "batched")
	e.pool.Exec(context.Background(), `UPDATE orders SET updated_at = NOW() - interval '1 minute' WHERE id = $1`, orderID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.lifecycle.StartSweep(ctx)

	deadline := time.Now().Add(2*config.SweepInterval + 2*time.Second)
	for time.Now().Before(deadline) {
		if e.orderStatus(t, orderID) == "queued" {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("order still %s, want queued", e.orderStatus(t, orderID))
}

func (e *testEnv) claimValue(courierID string) string {
	v, _ := e.redis.Get(context.Background(), "route:claim:"+courierID).Result()
	return v
}

func (e *testEnv) acceptedAssignmentID(t *testing.T, orderID string) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(), `SELECT id FROM assignments WHERE order_id = $1`, orderID).Scan(&id); err != nil {
		t.Fatalf("no assignment for %s: %v", orderID, err)
	}
	return id
}

// Redis lost the claims (restart, flush): reconciliation puts back one per
// pending offer and one per active assignment.
func TestReconcileRestoresLostClaims(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	_, pendingCourier, pendingOffer := e.openOffer(t)
	orderID, assignedCourier, offerID := e.openOffer(t)
	if err := e.lifecycle.Resolve(ctx, assignedCourier, offerID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	assignmentID := e.acceptedAssignmentID(t, orderID)

	e.redis.FlushDB(ctx)
	e.lifecycle.ReconcileClaims(ctx)

	if got := e.claimValue(pendingCourier); got != pendingOffer {
		t.Errorf("pending offer claim = %q, want %q", got, pendingOffer)
	}
	if got := e.claimValue(assignedCourier); got != assignmentID {
		t.Errorf("assignment claim = %q, want %q", got, assignmentID)
	}
	ttl, _ := e.redis.PTTL(ctx, "route:claim:"+assignedCourier).Result()
	if ttl <= 0 {
		t.Errorf("restored assignment claim has no TTL: %v", ttl)
	}
}

// A failed conversion on accept leaves the offer claim on a courier who now
// holds an assignment; reconciliation turns it into the assignment claim.
func TestReconcileUpgradesStaleOfferClaim(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orderID, courierID, offerID := e.openOffer(t)
	if err := e.lifecycle.Resolve(ctx, courierID, offerID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	e.redis.Set(ctx, "route:claim:"+courierID, offerID, time.Minute) // as if conversion failed

	e.lifecycle.ReconcileClaims(ctx)

	if got, want := e.claimValue(courierID), e.acceptedAssignmentID(t, orderID); got != want {
		t.Errorf("claim = %q, want assignment %q", got, want)
	}
}

// A claim held under some unrelated value is reported, never overwritten.
func TestReconcileLeavesForeignClaimAlone(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	_, courierID, _ := e.openOffer(t)
	e.redis.Set(ctx, "route:claim:"+courierID, "someone-elses", time.Minute)

	e.lifecycle.ReconcileClaims(ctx)

	if got := e.claimValue(courierID); got != "someone-elses" {
		t.Errorf("claim overwritten: %q", got)
	}
}

// Postgres refuses to offer to a courier that is already committed, even
// when Redis has forgotten (so the claim looked free).
func TestCreateOfferRefusesBusyCourier(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	// Pending offer already out to the courier.
	_, courierID, offerID := e.openOffer(t)
	second := e.insertOrder(t, "batched")
	err := e.lifecycle.CreateOffer(ctx, uuid.New().String(), second, courierID, 1.0, 0)
	if err != offers.ErrCourierBusy {
		t.Fatalf("pending offer: got %v, want ErrCourierBusy", err)
	}
	if got := e.orderStatus(t, second); got != "batched" {
		t.Errorf("refused order status = %s, want batched", got)
	}

	// Active assignment.
	if err := e.lifecycle.Resolve(ctx, courierID, offerID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}
	err = e.lifecycle.CreateOffer(ctx, uuid.New().String(), second, courierID, 1.0, 0)
	if err != offers.ErrCourierBusy {
		t.Fatalf("active assignment: got %v, want ErrCourierBusy", err)
	}
	var n int
	e.pool.QueryRow(ctx, `SELECT count(*) FROM offers WHERE order_id = $1`, second).Scan(&n)
	if n != 0 {
		t.Errorf("%d offer rows leaked for the refused order", n)
	}
}

// Reconciliation reads the live offers and assignments, then restores their
// claims. If one finishes in between, its release has already run, so the
// restore would create a claim for something that no longer exists. For an
// assignment that claim would last an hour. Reconciliation must notice and
// undo it. The load test found this: 34 claims restored in a run with no faults.
func TestReconcileDoesNotResurrectAFinishedAssignmentsClaim(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	orderID, courierID, offerID := e.openOffer(t)
	if err := e.lifecycle.Resolve(ctx, courierID, offerID, true); err != nil {
		t.Fatalf("accept: %v", err)
	}

	e.lifecycle.SetReconcileHook(func() { e.lifecycle.Complete(ctx, orderID, courierID) })
	e.lifecycle.ReconcileClaims(ctx)

	if got := e.claimValue(courierID); got != "" {
		t.Fatalf("a claim %q was left on a courier whose assignment had completed", got)
	}
}

func TestReconcileDoesNotResurrectAResolvedOffersClaim(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	_, courierID, offerID := e.openOffer(t)

	e.lifecycle.SetReconcileHook(func() { e.lifecycle.Resolve(ctx, courierID, offerID, false) })
	e.lifecycle.ReconcileClaims(ctx)

	if got := e.claimValue(courierID); got != "" {
		t.Fatalf("a claim %q was left on a courier whose offer had been declined", got)
	}
}
