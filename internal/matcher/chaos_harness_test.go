package matcher_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/metadata"

	"route/internal/api/proto"
	"route/internal/auth"
	"route/internal/claims"
	"route/internal/config"
	"route/internal/index"
	"route/internal/ingest"
	"route/internal/matcher"
	"route/internal/offers"
	"route/internal/surge"
)

type behavior int32

const (
	accepts behavior = iota
	declines
	silent // never answers an offer
)

// simCourier is a scripted courier: it pings on a short interval and answers
// offers according to its behaviour, over the real ingest handler.
type simCourier struct {
	id     string
	stream *mockCourierStream
	mode   atomic.Int32
	paused atomic.Bool // stop pinging (goes stale) without disconnecting
	skew   time.Duration

	lat, lng float64
	stop     chan struct{}
	done     chan struct{}
	once     sync.Once
}

func (c *simCourier) send(m *proto.CourierMessage) {
	select {
	case c.stream.recvCh <- m:
	case <-c.stop:
	}
}

func (c *simCourier) loop() {
	defer close(c.done)
	tick := time.NewTicker(40 * time.Millisecond)
	defer tick.Stop()
	seq := int64(0)
	ping := func() {
		seq++
		c.send(&proto.CourierMessage{Payload: &proto.CourierMessage_Ping{Ping: &proto.Ping{
			CourierId: c.id, Lat: c.lat, Lng: c.lng, Heading: 90, Seq: seq,
			ClientTs: time.Now().Add(c.skew).UnixMilli(),
		}}})
	}
	ping()
	for {
		select {
		case <-c.stop:
			return
		case <-tick.C:
			if !c.paused.Load() {
				ping()
			}
		case msg := <-c.stream.sendCh:
			if offer := msg.GetOffer(); offer != nil {
				switch behavior(c.mode.Load()) {
				case accepts, declines:
					c.send(&proto.CourierMessage{Payload: &proto.CourierMessage_OfferResponse{
						OfferResponse: &proto.OfferResponse{
							OfferId: offer.OfferId,
							Accept:  behavior(c.mode.Load()) == accepts,
						}}})
				}
			}
		}
	}
}

// disconnect ends the courier's gRPC session, like a dropped connection.
func (c *simCourier) disconnect() {
	c.once.Do(func() {
		close(c.stop)
		<-c.done
		close(c.stream.recvCh)
	})
}

type harness struct {
	t    *testing.T
	ctx  context.Context
	pool *pgxpool.Pool
	rdb  *redis.Client // direct to the real Redis, for assertions and fault injection

	store     *index.Store // matcher/claims path (may go through a fault proxy)
	pingStore *index.Store // always direct: pings are not the system under test
	claims    *claims.Manager
	registry  *ingest.StreamRegistry
	lifecycle *offers.Lifecycle
	batcher   *matcher.Batcher
	surge     *surge.Engine
	authn     *auth.Auth
	handler   *ingest.Handler

	mu          sync.Mutex
	couriers    map[string]*simCourier
	sweepCancel context.CancelFunc
}

func redisAddr() string {
	if a := os.Getenv("REDIS_ADDR"); a != "" {
		return a
	}
	return "localhost:6379"
}

// newHarness wires the real matcher, lifecycle and ingest handler against the
// test Postgres and Redis. matcherRedis, if non-empty, is the address the
// matcher and lifecycle use to reach Redis (a fault-injecting proxy).
func newHarness(t *testing.T, matcherRedis string, staleTTL time.Duration) *harness {
	t.Helper()
	ctx := context.Background()

	pgUrl := os.Getenv("POSTGRES_URL")
	if pgUrl == "" {
		pgUrl = "postgres://route:routepassword@localhost:5432/route?sslmode=disable"
	}
	pool, err := pgxpool.New(ctx, pgUrl)
	if err != nil {
		t.Fatalf("db connect failed: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skip("Postgres not running, skipping test")
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr()})
	if err := rdb.Ping(ctx).Err(); err != nil {
		rdb.Close()
		pool.Close()
		t.Skip("Redis not running, skipping test")
	}
	pool.Exec(ctx, "TRUNCATE orders, assignments, offers CASCADE")
	rdb.FlushDB(ctx)

	if matcherRedis == "" {
		matcherRedis = redisAddr()
	}
	oldWindow := config.BatchWindow
	config.BatchWindow = 100 * time.Millisecond

	h := &harness{t: t, ctx: ctx, pool: pool, rdb: rdb, couriers: map[string]*simCourier{}}
	h.store = index.NewStore(matcherRedis, staleTTL)
	h.pingStore = index.NewStore(redisAddr(), staleTTL)
	h.claims = claims.NewManager(h.store.GetClient())
	h.registry = ingest.NewStreamRegistry()
	h.surge = surge.NewEngine(pool, h.store, &mockPublisher{})
	h.batcher = matcher.NewBatcher(pool, h.store, h.claims, nil, nil, h.surge)
	h.lifecycle = offers.NewLifecycle(pool, h.claims, h.registry, h.batcher, &mockOutbox{})
	h.batcher.SetLifecycle(h.lifecycle)
	h.authn = auth.New("chaos-test-api-token-0123")
	h.handler = ingest.NewHandler(h.pingStore, &mockHistory{}, h.registry, h.lifecycle, h.authn, 5000)

	t.Cleanup(func() {
		if h.sweepCancel != nil {
			h.sweepCancel()
		}
		h.mu.Lock()
		for _, c := range h.couriers {
			c.disconnect()
		}
		h.mu.Unlock()
		h.batcher.Stop()
		h.surge.Stop()
		h.store.Close()
		h.pingStore.Close()
		rdb.Close()
		pool.Close()
		config.BatchWindow = oldWindow
	})
	return h
}

func (h *harness) addCourier(id string, mode behavior, lat, lng float64, skew time.Duration) *simCourier {
	c := &simCourier{
		id: id, lat: lat, lng: lng, skew: skew,
		stop: make(chan struct{}), done: make(chan struct{}),
		stream: newMockCourierStream(metadata.NewIncomingContext(context.Background(),
			auth.CourierMetadata(id, h.authn.CourierToken(id)))),
	}
	c.mode.Store(int32(mode))
	h.mu.Lock()
	h.couriers[id] = c
	h.mu.Unlock()
	go h.handler.Session(c.stream)
	go c.loop()
	return c
}

func (h *harness) startSweep() {
	ctx, cancel := context.WithCancel(h.ctx)
	h.sweepCancel = cancel
	h.lifecycle.StartSweep(ctx)
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatalf("exec %q: %v", sql, err)
	}
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

func (h *harness) insertOrder(status string, lat, lng float64) string {
	h.t.Helper()
	id := uuid.New().String()
	h.exec(`INSERT INTO orders (id, idempotency_key, pickup, dropoff, pickup_h3, status)
		VALUES ($1, $2, ST_SetSRID(ST_MakePoint($4, $3), 4326), ST_SetSRID(ST_MakePoint($4, $3), 4326), 'h3', $5)`,
		id, uuid.New().String(), lat, lng, status)
	return id
}

func (h *harness) orderStatus(id string) string {
	h.t.Helper()
	var s string
	if err := h.pool.QueryRow(h.ctx, `SELECT status FROM orders WHERE id = $1`, id).Scan(&s); err != nil {
		h.t.Fatalf("order status: %v", err)
	}
	return s
}

func (h *harness) queuedOrderIDs() []string {
	rows, err := h.pool.Query(h.ctx, `SELECT id FROM orders WHERE status = 'queued' ORDER BY created_at, id`)
	if err != nil {
		h.t.Fatalf("queued orders: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	return ids
}

func (h *harness) eventually(timeout time.Duration, what string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("timed out after %v waiting for: %s", timeout, what)
}

func (h *harness) waitIndexed(n int) {
	h.t.Helper()
	h.eventually(5*time.Second, fmt.Sprintf("%d couriers indexed", n), func() bool {
		got, _ := h.rdb.ZCard(h.ctx, "route:couriers").Result()
		return got >= int64(n)
	})
}

// expireOverdueOffers moves the deadline of pending offers older than minAge
// into the past, standing in for the 15s offer deadline passing.
func (h *harness) expireOverdueOffers(minAge time.Duration) {
	h.pool.Exec(h.ctx, `UPDATE offers SET deadline_at = NOW() - interval '1 second'
		WHERE state = 'pending' AND created_at < NOW() - ($1 * interval '1 millisecond')`, minAge.Milliseconds())
}

// expireOverdue runs the offer-expiry half of the sweep once, for tests that
// drive recovery by hand instead of starting the sweep goroutine.
func (h *harness) expireOverdue() {
	rows, err := h.pool.Query(h.ctx, `SELECT id FROM offers WHERE state = 'pending' AND deadline_at < NOW()`)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		h.lifecycle.ExpireOffer(h.ctx, id)
	}
}

// watchDoubleBooking polls for the core invariant: no courier holds more than
// one non-terminal offer or active assignment at any instant. The returned
// function stops the watcher and reports any violations it saw.
func (h *harness) watchDoubleBooking() func() []string {
	stop := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var violations []string
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			rows, err := h.pool.Query(h.ctx, `
				SELECT courier_id, count(*) FROM (
					SELECT courier_id FROM offers WHERE state = 'pending'
					UNION ALL
					SELECT courier_id FROM assignments WHERE completed_at IS NULL
				) held GROUP BY courier_id HAVING count(*) > 1`)
			if err != nil {
				continue
			}
			for rows.Next() {
				var id string
				var n int
				rows.Scan(&id, &n)
				mu.Lock()
				violations = append(violations, fmt.Sprintf("courier %s holds %d non-terminal offers/assignments", id, n))
				mu.Unlock()
			}
			rows.Close()
		}
	}()
	return func() []string {
		close(stop)
		<-done
		mu.Lock()
		defer mu.Unlock()
		return violations
	}
}

// claimValues returns courier id -> claim value for every claim in Redis.
func (h *harness) claimValues() map[string]string {
	keys, _ := h.rdb.Keys(h.ctx, "route:claim:*").Result()
	out := make(map[string]string, len(keys))
	for _, k := range keys {
		if v, err := h.rdb.Get(h.ctx, k).Result(); err == nil {
			out[k[len("route:claim:"):]] = v
		}
	}
	return out
}

// checkClaimsConsistent asserts the claim bookkeeping is sound once the
// system has settled:
//   - every active assignment's courier holds a claim naming that assignment
//   - every pending offer's courier holds a claim naming that offer
//   - any other claim is a short-lived offer claim, not a long-lived orphan
//     (a claim that outlives every offer would block a courier for up to
//     AssignmentClaimTTL)
func (h *harness) checkClaimsConsistent() {
	h.t.Helper()
	valid := map[string]bool{}
	want := map[string]string{} // courier -> claim value Postgres says must exist

	rows, _ := h.pool.Query(h.ctx, `SELECT id, courier_id FROM assignments WHERE completed_at IS NULL`)
	for rows.Next() {
		var id, courier string
		rows.Scan(&id, &courier)
		valid[id] = true
		want[courier] = id
	}
	rows.Close()
	rows, _ = h.pool.Query(h.ctx, `SELECT id, courier_id FROM offers WHERE state = 'pending'`)
	for rows.Next() {
		var id, courier string
		rows.Scan(&id, &courier)
		valid[id] = true
		want[courier] = id
	}
	rows.Close()

	have := h.claimValues()
	for courier, value := range want {
		if have[courier] != value {
			h.t.Errorf("courier %s: Postgres says claim %q, Redis has %q", courier, value, have[courier])
		}
	}
	for courier, value := range have {
		if valid[value] {
			continue
		}
		ttl, _ := h.rdb.PTTL(h.ctx, "route:claim:"+courier).Result()
		if ttl < 0 || ttl > config.OfferClaimTTL+2*time.Second {
			h.t.Errorf("courier %s: orphan claim %q with TTL %v (no offer or assignment owns it)", courier, value, ttl)
		}
	}
}
