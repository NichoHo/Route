package matcher_test

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"route/internal/config"
	"route/internal/metrics"
)

var (
	centerLat = config.JakartaCenterLat
	centerLng = config.JakartaCenterLng
)

// ROUTE_SPEC.md 10.1: across N concurrent batch solves against a shared
// courier pool, no courier holds more than one non-terminal offer or active
// assignment at any instant. 50 concurrent solvers, 100 couriers, all
// competing for the same nearest candidates.
func TestConcurrentSolversNoDoubleBooking(t *testing.T) {
	const couriers, orders, perSolver = 100, 200, 4
	h := newHarness(t, "", 5*time.Second)
	for i := 0; i < couriers; i++ {
		h.addCourier(fmt.Sprintf("a1-%03d", i), accepts,
			centerLat+float64(i%10)*0.0002, centerLng+float64(i/10)*0.0002, 0)
	}
	h.waitIndexed(couriers)
	for i := 0; i < orders; i++ {
		h.insertOrder("queued", centerLat, centerLng)
	}

	stopWatch := h.watchDoubleBooking()
	conflictsBefore := metrics.ClaimConflicts()
	maxSolvers := 0

	assigned := func() int { return h.count(`SELECT count(*) FROM assignments`) }
	for round := 0; round < 40 && assigned() < couriers; round++ {
		queued := h.queuedOrderIDs()
		if len(queued) == 0 {
			break
		}
		// Every solver starts at the same instant.
		start := make(chan struct{})
		var wg sync.WaitGroup
		solvers := 0
		for i := 0; i < len(queued); i += perSolver {
			end := i + perSolver
			if end > len(queued) {
				end = len(queued)
			}
			solvers++
			wg.Add(1)
			go func(ids []string) {
				defer wg.Done()
				<-start
				h.batcher.SolveBatch(h.ctx, ids, time.Time{})
			}(queued[i:end])
		}
		if solvers > maxSolvers {
			maxSolvers = solvers
		}
		close(start)
		wg.Wait()
		h.eventually(10*time.Second, "round's offers to resolve", func() bool {
			return h.count(`SELECT count(*) FROM offers WHERE state = 'pending'`) == 0
		})
	}

	if violations := stopWatch(); len(violations) > 0 {
		t.Fatalf("double booking observed: %v", violations[:min(5, len(violations))])
	}
	if maxSolvers < 50 {
		t.Errorf("only %d concurrent solvers ran, wanted 50", maxSolvers)
	}
	if got := assigned(); got != couriers {
		t.Fatalf("assigned %d orders, want exactly %d (one per courier)", got, couriers)
	}
	if n := h.count(`SELECT count(DISTINCT courier_id) FROM assignments`); n != couriers {
		t.Errorf("%d distinct couriers hold assignments, want %d", n, couriers)
	}
	if conflicts := metrics.ClaimConflicts() - conflictsBefore; conflicts == 0 {
		t.Error("no claim conflicts occurred, so the test did not exercise contention")
	}
	if n := h.count(`SELECT count(*) FROM orders WHERE priority > 0`); n == 0 {
		t.Error("orders that lost claim races should have had their priority boosted")
	}
	h.checkClaimsConsistent()
	if n := len(h.claimValues()); n != couriers {
		t.Errorf("%d claims in Redis, want %d (one per assignment)", n, couriers)
	}
}

// ROUTE_SPEC.md 10.1 and 10.2: every created order terminates in exactly one
// of assigned, exhausted or cancelled, whatever the couriers do. Couriers
// accept, decline or never answer; some orders are cancelled mid-flight.
func TestNoLostOrdersWithMixedCourierBehaviour(t *testing.T) {
	const couriers, orders = 80, 40
	h := newHarness(t, "", 5*time.Second)
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < couriers; i++ {
		mode := accepts
		switch r := rng.Float64(); {
		case r < 0.2:
			mode = declines
		case r < 0.4:
			mode = silent
		}
		h.addCourier(fmt.Sprintf("lo-%02d", i), mode,
			centerLat+rng.Float64()*0.01, centerLng+rng.Float64()*0.01, 0)
	}
	h.waitIndexed(couriers)
	h.startSweep()
	stopWatch := h.watchDoubleBooking()

	ids := make([]string, orders)
	for i := range ids {
		ids[i] = h.insertOrder("queued", centerLat+rng.Float64()*0.01, centerLng+rng.Float64()*0.01)
		h.batcher.Trigger(ids[i])
	}
	time.Sleep(300 * time.Millisecond)
	for _, i := range rng.Perm(orders)[:5] {
		h.lifecycle.CancelOrder(h.ctx, ids[i]) // fails harmlessly if already assigned
	}

	terminal := func() int {
		return h.count(`SELECT count(*) FROM orders WHERE status IN ('assigned', 'exhausted', 'cancelled')`)
	}
	h.eventually(90*time.Second, "every order terminal", func() bool {
		h.expireOverdueOffers(1500 * time.Millisecond) // silent couriers' deadlines pass
		for _, id := range h.queuedOrderIDs() {
			h.batcher.Trigger(id)
		}
		return terminal() == orders
	})

	if violations := stopWatch(); len(violations) > 0 {
		t.Fatalf("double booking observed: %v", violations[:min(5, len(violations))])
	}
	check := func(what, sql string) {
		t.Helper()
		if n := h.count(sql); n != 0 {
			t.Errorf("%s: %d violations", what, n)
		}
	}
	check("order with more than max_attempts offers",
		fmt.Sprintf(`SELECT count(*) FROM (SELECT 1 FROM offers GROUP BY order_id HAVING count(*) > %d) x`, config.MaxAttempts))
	check("order re-offered to a courier it already had",
		`SELECT count(*) FROM (SELECT 1 FROM offers GROUP BY order_id HAVING count(*) <> count(DISTINCT courier_id)) x`)
	check("assigned order without exactly one assignment",
		`SELECT count(*) FROM orders o WHERE status = 'assigned' AND (SELECT count(*) FROM assignments a WHERE a.order_id = o.id) <> 1`)
	check("exhausted or cancelled order that holds an assignment",
		`SELECT count(*) FROM orders o WHERE status IN ('exhausted', 'cancelled') AND EXISTS (SELECT 1 FROM assignments a WHERE a.order_id = o.id)`)
	check("exhausted order with fewer than max_attempts attempts",
		fmt.Sprintf(`SELECT count(*) FROM orders WHERE status = 'exhausted' AND attempts < %d`, config.MaxAttempts))
	check("pending offer left behind", `SELECT count(*) FROM offers WHERE state = 'pending'`)

	h.eventually(5*time.Second, "claims settle", func() bool {
		return len(h.claimValues()) == h.count(`SELECT count(*) FROM assignments WHERE completed_at IS NULL`)
	})
	h.checkClaimsConsistent()
}

// ROUTE_SPEC.md 10.5: kill Redis mid-batch. The cut point is swept across a
// whole batch (about 31 Redis round trips: candidate gathering, then claims,
// then the accept-time claim conversions), so some runs lose a GEOSEARCH, some
// a claim whose SET NX already ran, some a conversion whose reply was lost.
//
// Variants:
//   - Redis keeps its data (the connection dropped).
//   - Redis returns empty (restarted without persistence), and nothing
//     reconciles claims yet: only the Postgres-side guards stand between a
//     busy courier and a second offer.
//   - Redis returns empty and claims are reconciled from Postgres straight away.
//
// Asserted: the in-flight batch is abandoned and its orders go back to the
// queue, no courier is ever double booked, every order still gets assigned,
// and (after the periodic reconcile) the claim bookkeeping is sound.
func TestChaosRedisKilledMidBatch(t *testing.T) {
	// How many Redis round trips does a whole batch make, accepts included?
	// Measure it with a clean run, so the cut points cover the whole batch
	// however the code changes. (This was once a hard-coded list that went
	// stale when the candidate search was batched.)
	var total int
	t.Run("calibration", func(t *testing.T) { total = int(chaosRedisKill(t, 1000, false, false)) })
	if total < 3 {
		t.Fatalf("a batch made %d Redis round trips; the sweep would not mean anything", total)
	}
	t.Logf("a clean batch makes %d Redis round trips; cutting Redis after each one", total)

	variants := []struct {
		name         string
		flush, early bool
	}{
		{"keeps-data", false, false},
		{"lost-data", true, false},
		{"lost-data-reconciled", true, true},
	}
	for _, v := range variants {
		for killAfter := 0; killAfter <= total+1; killAfter++ {
			t.Run(fmt.Sprintf("%s/killAfter=%d", v.name, killAfter), func(t *testing.T) {
				chaosRedisKill(t, killAfter, v.flush, v.early)
			})
		}
	}
}

// chaosRedisKill runs one cut-and-recover scenario and returns how many Redis
// round trips the batch made.
func chaosRedisKill(t *testing.T, killAfter int, flush, reconcileEarly bool) int64 {
	const couriers, orders = 8, 3
	proxy := newTCPProxy(t, redisAddr())
	h := newHarness(t, proxy.Addr(), 15*time.Second)
	stopWatch := h.watchDoubleBooking()

	for i := 0; i < couriers; i++ {
		h.addCourier(fmt.Sprintf("kc-%d", i), accepts, centerLat+float64(i)*0.0003, centerLng, 0)
	}
	h.waitIndexed(couriers)
	ids := make([]string, orders)
	for i := range ids {
		ids[i] = h.insertOrder("queued", centerLat, centerLng)
	}

	proxy.KillAfter(killAfter)
	done := make(chan struct{})
	go func() {
		h.batcher.SolveBatch(h.ctx, ids, time.Time{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("SolveBatch hung while Redis was down")
	}
	proxy.Kill()                       // if the budget was not reached, kill it now
	time.Sleep(300 * time.Millisecond) // let in-flight accepts finish or fail
	roundTrips := proxy.sent.Load()
	t.Logf("kill landed mid-batch=%v after %d round trips; orders: queued=%d offered=%d assigned=%d; pending offers=%d",
		proxy.fired.Load(), proxy.sent.Load(),
		h.count(`SELECT count(*) FROM orders WHERE status = 'queued'`),
		h.count(`SELECT count(*) FROM orders WHERE status = 'offered'`),
		h.count(`SELECT count(*) FROM orders WHERE status = 'assigned'`),
		h.count(`SELECT count(*) FROM offers WHERE state = 'pending'`))

	// The batch is over. Nothing may be left half-batched, and the offer and
	// order tables must still agree. An order that lost out is retried in the
	// next window, so it can be mid-flight in that retry for a moment; what
	// must never happen is an order that stays 'batched'.
	stuck := func() int { return h.count(`SELECT count(*) FROM orders WHERE status = 'batched'`) }
	h.eventually(5*time.Second, "no order left in 'batched' after the batch was abandoned", func() bool { return stuck() == 0 })
	if n := h.count(`SELECT count(*) FROM orders o WHERE status = 'offered'
		AND (SELECT count(*) FROM offers f WHERE f.order_id = o.id AND f.state = 'pending') <> 1`); n != 0 {
		t.Errorf("%d offered orders without exactly one pending offer", n)
	}
	if n := h.count(`SELECT count(*) FROM offers f WHERE state = 'pending'
		AND NOT EXISTS (SELECT 1 FROM orders o WHERE o.id = f.order_id AND o.status = 'offered')`); n != 0 {
		t.Errorf("%d pending offers whose order is not 'offered'", n)
	}

	// Recovery.
	if flush {
		h.rdb.FlushDB(h.ctx)
	}
	proxy.Restart()
	if reconcileEarly {
		h.lifecycle.ReconcileClaims(h.ctx)
	}
	h.eventually(60*time.Second, "all orders assigned after recovery", func() bool {
		h.expireOverdueOffers(1500 * time.Millisecond) // offers whose accept was lost
		h.expireOverdue()
		for _, id := range h.queuedOrderIDs() {
			h.batcher.Trigger(id)
		}
		return h.count(`SELECT count(*) FROM orders WHERE status = 'assigned'`) == orders
	})

	if violations := stopWatch(); len(violations) > 0 {
		t.Fatalf("double booking after Redis recovery: %v", violations[:min(5, len(violations))])
	}
	if n := h.count(`SELECT count(*) FROM (SELECT 1 FROM assignments WHERE completed_at IS NULL GROUP BY courier_id HAVING count(*) > 1) x`); n != 0 {
		t.Errorf("%d couriers hold more than one active assignment", n)
	}
	h.eventually(5*time.Second, "no pending offers", func() bool {
		return h.count(`SELECT count(*) FROM offers WHERE state = 'pending'`) == 0
	})
	// The periodic reconcile (every 30s in production) has run by now.
	h.lifecycle.ReconcileClaims(h.ctx)
	h.checkClaimsConsistent()
	return roundTrips
}

// ROUTE_SPEC.md 10.5: kill routed mid-batch. A crash leaves orders in
// 'batched'. A fresh process (sweep plus batcher) must return them to the
// queue and match them.
func TestChaosRoutedCrashMidBatch(t *testing.T) {
	const orders = 6
	h := newHarness(t, "", 5*time.Second)
	for i := 0; i < 10; i++ {
		h.addCourier(fmt.Sprintf("rc-%d", i), accepts, centerLat+float64(i)*0.0003, centerLng, 0)
	}
	h.waitIndexed(10)
	stopWatch := h.watchDoubleBooking()

	for i := 0; i < orders; i++ {
		id := h.insertOrder("batched", centerLat, centerLng)
		h.exec(`UPDATE orders SET updated_at = NOW() - interval '1 minute' WHERE id = $1`, id)
	}

	h.startSweep() // the restarted process
	h.eventually(30*time.Second, "stranded orders matched after restart", func() bool {
		return h.count(`SELECT count(*) FROM orders WHERE status = 'assigned'`) == orders
	})
	if v := stopWatch(); len(v) > 0 {
		t.Fatalf("double booking: %v", v)
	}
	h.checkClaimsConsistent()
}

// ROUTE_SPEC.md 10.5: a courier disconnects while holding a pending offer.
// The offer expires, the claim is released, and the order is re-queued with
// that courier excluded and goes to someone else.
func TestChaosCourierDisconnectsHoldingOffer(t *testing.T) {
	h := newHarness(t, "", 5*time.Second)
	a := h.addCourier("dc-a", silent, centerLat, centerLng, 0)
	h.addCourier("dc-b", accepts, centerLat+0.03, centerLng, 0) // farther, so a is offered first
	h.waitIndexed(2)

	orderID := h.insertOrder("queued", centerLat, centerLng)
	h.batcher.Trigger(orderID)
	h.eventually(10*time.Second, "offer to dc-a", func() bool {
		return h.count(`SELECT count(*) FROM offers WHERE order_id = $1 AND courier_id = 'dc-a' AND state = 'pending'`, orderID) == 1
	})

	a.disconnect()
	if _, held := h.claimValues()["dc-a"]; !held {
		t.Fatal("claim should be held while the offer is still pending")
	}

	h.startSweep()
	h.eventually(15*time.Second, "offer expired and claim released", func() bool {
		h.expireOverdueOffers(500 * time.Millisecond)
		_, held := h.claimValues()["dc-a"]
		return !held && h.count(`SELECT count(*) FROM offers WHERE order_id = $1 AND courier_id = 'dc-a' AND state = 'expired'`, orderID) == 1
	})
	h.eventually(15*time.Second, "order assigned to dc-b", func() bool {
		return h.count(`SELECT count(*) FROM assignments WHERE order_id = $1 AND courier_id = 'dc-b'`, orderID) == 1
	})
	if n := h.count(`SELECT count(*) FROM orders WHERE id = $1 AND 'dc-a' = ANY(excluded_couriers)`, orderID); n != 1 {
		t.Error("dc-a should be excluded from the order after its offer expired")
	}
	h.checkClaimsConsistent()
}

// ROUTE_SPEC.md 10.5: client clocks +-30s off server time must not matter.
// Staleness and deadlines run on server time only.
func TestChaosClockSkew(t *testing.T) {
	h := newHarness(t, "", 5*time.Second)
	h.addCourier("sk-ahead", accepts, centerLat, centerLng, 30*time.Second)
	h.addCourier("sk-behind", accepts, centerLat+0.0003, centerLng, -30*time.Second)
	h.waitIndexed(2)

	got, err := h.store.FindCandidates(h.ctx, centerLat, centerLng, 10, 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("skewed couriers must be candidates, got %v, %v", got, err)
	}
	for i := 0; i < 2; i++ {
		h.batcher.Trigger(h.insertOrder("queued", centerLat, centerLng))
	}
	h.eventually(15*time.Second, "both orders assigned", func() bool {
		return h.count(`SELECT count(DISTINCT courier_id) FROM assignments`) == 2
	})
}

// ROUTE_SPEC.md 10.2: a courier whose seen key has expired is never a
// candidate, even if it is the nearest.
func TestStaleCourierNeverOffered(t *testing.T) {
	h := newHarness(t, "", 400*time.Millisecond)
	stale := h.addCourier("st-stale", accepts, centerLat, centerLng, 0) // nearest
	h.addCourier("st-live", accepts, centerLat+0.02, centerLng, 0)
	h.waitIndexed(2)
	stale.paused.Store(true)

	h.eventually(5*time.Second, "stale courier drops out of candidates", func() bool {
		got, _ := h.store.FindCandidates(h.ctx, centerLat, centerLng, 10, 1)
		return len(got) == 1 && got[0].ID == "st-live"
	})
	orderID := h.insertOrder("queued", centerLat, centerLng)
	h.batcher.Trigger(orderID)
	h.eventually(10*time.Second, "order assigned", func() bool {
		return h.count(`SELECT count(*) FROM assignments WHERE order_id = $1 AND courier_id = 'st-live'`, orderID) == 1
	})
	if n := h.count(`SELECT count(*) FROM offers WHERE courier_id = 'st-stale'`); n != 0 {
		t.Errorf("stale courier received %d offers", n)
	}
}
