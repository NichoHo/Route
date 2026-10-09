package matcher_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"route/internal/config"
)

// addHistory gives a courier n resolved offers in the given state, as the
// matcher reads them to score the courier's accept rate.
func (h *harness) addHistory(courierID, state string, n int) {
	for i := 0; i < n; i++ {
		orderID := h.insertOrder("exhausted", centerLat, centerLng)
		h.exec(`INSERT INTO offers (id, order_id, courier_id, state, cost, deadline_at, resolved_at)
			VALUES ($1, $2, $3, $4, 1, NOW(), NOW())`, uuid.New().String(), orderID, courierID, state)
	}
}

func (h *harness) offersFor(orderID string) int {
	return h.count(`SELECT count(*) FROM offers WHERE order_id = $1`, orderID)
}

func (h *harness) solve(orderIDs ...string) {
	h.batcher.SolveBatch(h.ctx, orderIDs, time.Time{})
}

// With fewer couriers than orders, the terms that belong to the order decide
// who gets served. Couriers never answer here (silent), so the offer stays
// pending and nothing else moves.

func TestScoringScarceCourierGoesToOldestOrder(t *testing.T) {
	// Run with the orders listed both ways round, so a tie between them
	// cannot pass by luck of list order.
	for _, newestFirst := range []bool{true, false} {
		name := "oldest listed first"
		if newestFirst {
			name = "newest listed first"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", 5*time.Second)
			h.addCourier("sc-1", silent, centerLat, centerLng, 0)
			h.waitIndexed(1)

			// The newer order is right at the courier; the older one is about
			// 200m away. Distance alone favours the newer order, so only the
			// wait term (10 per second left of 60, so 40s of age is worth
			// 400m) can make the older one win. With the wait weight at zero
			// this test fails rather than passing on a tie.
			newer := h.insertOrder("queued", centerLat, centerLng)
			older := h.insertOrder("queued", centerLat+0.0018, centerLng)
			h.exec(`UPDATE orders SET created_at = NOW() - interval '40 seconds' WHERE id = $1`, older)

			if newestFirst {
				h.solve(newer, older)
			} else {
				h.solve(older, newer)
			}

			if h.offersFor(older) != 1 || h.offersFor(newer) != 0 {
				t.Fatalf("offers: older=%d newer=%d, want the older order served", h.offersFor(older), h.offersFor(newer))
			}
			// The unserved order is retried in the next window, so it may be
			// queued or already batched again.
			if got := h.orderStatus(newer); got != "queued" && got != "batched" {
				t.Errorf("unserved order status = %s, want queued or batched", got)
			}
		})
	}
}

func TestScoringScarceCourierGoesToHigherPriorityOrder(t *testing.T) {
	for _, urgentFirst := range []bool{true, false} {
		name := "urgent listed last"
		if urgentFirst {
			name = "urgent listed first"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, "", 5*time.Second)
			h.addCourier("sp-1", silent, centerLat, centerLng, 0)
			h.waitIndexed(1)

			low := h.insertOrder("queued", centerLat, centerLng)
			urgent := h.insertOrder("queued", centerLat, centerLng)
			h.exec(`UPDATE orders SET priority = 9 WHERE id = $1`, urgent)
			h.exec(`UPDATE orders SET created_at = NOW() WHERE id IN ($1, $2)`, low, urgent) // equal age

			if urgentFirst {
				h.solve(urgent, low)
			} else {
				h.solve(low, urgent)
			}

			if h.offersFor(urgent) != 1 || h.offersFor(low) != 0 {
				t.Fatalf("offers: urgent=%d low=%d, want the priority 9 order served", h.offersFor(urgent), h.offersFor(low))
			}
		})
	}
}

// A courier with a record of declining loses to a reliable one even when
// slightly nearer. With the accept weight at zero, history is ignored.
func TestScoringPrefersReliableCourier(t *testing.T) {
	cases := []struct {
		name    string
		history bool
		weights func(w config.ScoringWeights) config.ScoringWeights
		want    string
	}{
		{"no history: nearer courier wins", false, nil, "flaky"},
		{"declines on record: reliable courier wins", true, nil, "reliable"},
		{"accept weight 0: history ignored", true, func(w config.ScoringWeights) config.ScoringWeights { w.Accept = 0; return w }, "flaky"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "", 5*time.Second)
			if c.weights != nil {
				h.batcher.SetWeights(c.weights(config.Scoring))
			}
			// flaky is about 100m from the order; reliable about 220m.
			h.addCourier("flaky", silent, centerLat+0.0009, centerLng, 0)
			h.addCourier("reliable", silent, centerLat-0.0020, centerLng, 0)
			h.waitIndexed(2)
			if c.history {
				h.addHistory("flaky", "declined", 8)
				h.addHistory("reliable", "accepted", 8)
			}

			orderID := h.insertOrder("queued", centerLat, centerLng)
			h.solve(orderID)

			var got string
			if err := h.pool.QueryRow(h.ctx, `SELECT courier_id FROM offers WHERE order_id = $1`, orderID).Scan(&got); err != nil {
				t.Fatalf("no offer made: %v", err)
			}
			if got != c.want {
				t.Fatalf("offered to %s, want %s", got, c.want)
			}
		})
	}
}

// Offers on orders that were cancelled are not the courier's fault.
func TestScoringIgnoresOffersOnCancelledOrders(t *testing.T) {
	h := newHarness(t, "", 5*time.Second)
	h.addCourier("flaky", silent, centerLat+0.0009, centerLng, 0)
	h.addCourier("other", silent, centerLat-0.0020, centerLng, 0)
	h.waitIndexed(2)
	for i := 0; i < 8; i++ {
		orderID := h.insertOrder("cancelled", centerLat, centerLng)
		h.exec(`INSERT INTO offers (id, order_id, courier_id, state, cost, deadline_at, resolved_at)
			VALUES ($1, $2, 'flaky', 'expired', 1, NOW(), NOW())`, uuid.New().String(), orderID)
	}

	orderID := h.insertOrder("queued", centerLat, centerLng)
	h.solve(orderID)

	if n := h.count(`SELECT count(*) FROM offers WHERE order_id = $1 AND courier_id = 'flaky'`, orderID); n != 1 {
		t.Fatal("cancelled-order expiries were counted against the nearer courier")
	}
}

// An order that loses out in a batch (more orders than couriers in reach) is
// retried in the next window. It must not wait for the sweep, which takes 5
// seconds or more. The sweep is not running in this test, so only the retry
// can serve the third order. The load test found the sweep wait as a 7.9 s p99
// match latency.
func TestLeftoverOrdersAreRetriedInTheNextWindow(t *testing.T) {
	h := newHarness(t, "", 5*time.Second)
	h.addCourier("lo-a", silent, centerLat, centerLng, 0)
	h.addCourier("lo-b", silent, centerLat+0.001, centerLng, 0)
	h.waitIndexed(2)

	orders := []string{
		h.insertOrder("queued", centerLat, centerLng),
		h.insertOrder("queued", centerLat, centerLng),
		h.insertOrder("queued", centerLat, centerLng),
	}
	h.solve(orders...)
	if n := h.count(`SELECT count(*) FROM offers WHERE state = 'pending'`); n != 2 {
		t.Fatalf("%d pending offers after the first window, want 2 (two couriers, three orders)", n)
	}

	// A third courier arrives. The leftover order must reach it without help.
	h.addCourier("lo-c", silent, centerLat+0.002, centerLng, 0)
	h.eventually(3*time.Second, "the leftover order to be offered to the new courier", func() bool {
		return h.count(`SELECT count(*) FROM offers WHERE state = 'pending'`) == 3
	})
}
