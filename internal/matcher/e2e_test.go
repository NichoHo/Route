package matcher_test

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"route/internal/api"
	"route/internal/config"
)

// ROUTE_SPEC.md 10.6, the one scripted end-to-end scenario: 50 couriers, 20
// orders over 30 seconds, one flaky courier, one hotspot. Orders go in through
// the real order API (bearer token, idempotency key); a WebSocket client
// watches the live dashboard feed the whole time.
//
// Asserted at the end:
//   - every order is terminal
//   - every courier is consistent between Redis and Postgres
//   - every batch recorded its batched-versus-greedy comparison
//   - the dashboard WebSocket received every assignment
func TestEndToEndScenario(t *testing.T) {
	if testing.Short() {
		t.Skip("takes about 45 seconds")
	}
	const couriers, orders, window = 50, 20, 30 * time.Second
	h := newHarness(t, "", 5*time.Second)
	rng := rand.New(rand.NewSource(10))

	// 50 couriers across the service area. Four decline every offer, so some
	// orders need a second offer.
	var flaky *simCourier
	for i := 0; i < couriers; i++ {
		mode := accepts
		if i%12 == 5 {
			mode = declines
		}
		lat := config.JakartaMinLat + rng.Float64()*(config.JakartaMaxLat-config.JakartaMinLat)
		lng := config.JakartaMinLng + rng.Float64()*(config.JakartaMaxLng-config.JakartaMinLng)
		c := h.addCourier(fmt.Sprintf("e2e-%02d", i), mode, lat, lng, 0)
		if i == 7 {
			flaky = c
		}
	}
	h.waitIndexed(couriers)
	h.startSweep()
	stopWatch := h.watchDoubleBooking()

	// Dashboard feed: real hub, real snapshotter, one WebSocket client.
	hub := api.NewHub(10)
	srv := httptest.NewServer(hub)
	defer srv.Close()
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	go api.NewSnapshotter(h.pool, h.store, h.surge).Run(ctx, hub)

	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial dashboard feed: %v", err)
	}
	defer conn.Close()
	var seenMu sync.Mutex
	seenAssigned := map[string]string{} // order id -> courier id, as the dashboard saw it
	snapshots := 0
	go func() {
		for {
			_, msg, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var snap struct {
				Orders []struct {
					ID        string `json:"id"`
					Status    string `json:"status"`
					CourierID string `json:"courier_id"`
				} `json:"orders"`
			}
			if json.Unmarshal(msg, &snap) != nil {
				continue
			}
			seenMu.Lock()
			snapshots++
			for _, o := range snap.Orders {
				if o.Status == "assigned" && o.CourierID != "" {
					seenAssigned[o.ID] = o.CourierID
				}
			}
			seenMu.Unlock()
		}
	}()

	// The order API, behind the same bearer-token guard as in routed.
	orderAPI := api.NewGuard(h.authn, 1000, 1000).Wrap(api.NewOrderAPI(h.pool, h.batcher, &mockOutbox{}, h.surge))
	hotspotLat, hotspotLng := -6.1951, 106.8230
	post := func(key string, lat, lng float64) (int, string) {
		body := fmt.Sprintf(`{"pickup_lat":%f,"pickup_lng":%f,"dropoff_lat":%f,"dropoff_lng":%f,"priority":1}`,
			lat, lng, math.Min(lat+0.005, config.JakartaMaxLat), lng)
		req := httptest.NewRequest("POST", "/orders", strings.NewReader(body))
		req.RemoteAddr = "10.0.0.1:1234"
		req.Header.Set("Authorization", "Bearer chaos-test-api-token-0123")
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		orderAPI.ServeHTTP(rec, req)
		var out struct {
			ID string `json:"id"`
		}
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out.ID
	}

	// Without the token the API refuses.
	if code, _ := (func() (int, string) {
		req := httptest.NewRequest("POST", "/orders", strings.NewReader("{}"))
		req.RemoteAddr = "10.0.0.2:1"
		rec := httptest.NewRecorder()
		orderAPI.ServeHTTP(rec, req)
		return rec.Code, ""
	})(); code != http.StatusUnauthorized {
		t.Fatalf("order without a token got %d, want 401", code)
	}

	// 20 orders over 30 seconds; 70% in the hotspot. The flaky courier loses
	// its GPS for a stretch in the middle, long enough to go stale.
	start := time.Now()
	ids := make([]string, 0, orders)
	for i := 0; i < orders; i++ {
		time.Sleep(time.Until(start.Add(time.Duration(i) * window / orders)))
		if i == 6 {
			flaky.paused.Store(true)
		}
		if i == 14 {
			flaky.paused.Store(false)
		}
		lat := config.JakartaMinLat + rng.Float64()*(config.JakartaMaxLat-config.JakartaMinLat)
		lng := config.JakartaMinLng + rng.Float64()*(config.JakartaMaxLng-config.JakartaMinLng)
		if rng.Float64() < 0.7 { // the hotspot: Gaussian, about 500m
			lat = hotspotLat + rng.NormFloat64()*500/111320
			lng = hotspotLng + rng.NormFloat64()*500/(111320*math.Cos(hotspotLat*math.Pi/180))
		}
		key := fmt.Sprintf("e2e-key-%d", i)
		code, id := post(key, lat, lng)
		if code != http.StatusCreated {
			t.Fatalf("order %d: got %d, want 201", i, code)
		}
		if i == 0 { // a repeated idempotency key returns the same order
			if code2, id2 := post(key, lat, lng); code2 != http.StatusOK || id2 != id {
				t.Fatalf("repeated key: got %d %q, want 200 %q", code2, id2, id)
			}
		}
		ids = append(ids, id)
	}

	// Every order must reach a terminal state.
	terminal := func() int {
		return h.count(`SELECT count(*) FROM orders WHERE status IN ('assigned', 'exhausted', 'cancelled')`)
	}
	h.eventually(60*time.Second, "all 20 orders terminal", func() bool {
		h.expireOverdueOffers(1500 * time.Millisecond)
		return terminal() == orders
	})
	if n := h.count(`SELECT count(*) FROM orders`); n != orders {
		t.Errorf("%d orders exist, want %d (the repeated key must not create one)", n, orders)
	}
	if n := h.count(`SELECT count(*) FROM orders WHERE status = 'exhausted'`); n != 0 {
		t.Errorf("%d orders exhausted with 50 couriers for 20 orders", n)
	}
	if v := stopWatch(); len(v) > 0 {
		t.Fatalf("double booking observed: %v", v[:min(5, len(v))])
	}

	// Redis and Postgres agree about every courier.
	h.eventually(5*time.Second, "claims settle", func() bool {
		return len(h.claimValues()) == h.count(`SELECT count(*) FROM assignments WHERE completed_at IS NULL`)
	})
	h.checkClaimsConsistent()
	if n := h.count(`SELECT count(*) FROM (SELECT 1 FROM assignments GROUP BY order_id HAVING count(*) <> 1) x`); n != 0 {
		t.Errorf("%d orders with more than one assignment", n)
	}

	// Every batch recorded the comparison between the optimal and greedy solutions.
	if n := h.count(`SELECT count(*) FROM batches`); n == 0 {
		t.Error("no batches were recorded")
	}
	if n := h.count(`SELECT count(*) FROM batches WHERE total_cost_batched IS NULL OR total_cost_greedy IS NULL
		OR total_distance_batched IS NULL OR total_distance_greedy IS NULL`); n != 0 {
		t.Errorf("%d batches without a batched-versus-greedy comparison", n)
	}
	if n := h.count(`SELECT coalesce(sum(order_count), 0) FROM batches`); n < orders {
		t.Errorf("batches cover %d order slots, want at least %d", n, orders)
	}

	// The dashboard feed showed every assignment. It samples at 4 Hz, so give
	// the last assignment a moment to appear.
	missing := func() []string {
		rows, _ := h.pool.Query(h.ctx, `SELECT o.id, a.courier_id FROM orders o JOIN assignments a ON a.order_id = o.id`)
		defer rows.Close()
		seenMu.Lock()
		defer seenMu.Unlock()
		var out []string
		for rows.Next() {
			var orderID, courierID string
			rows.Scan(&orderID, &courierID)
			if seenAssigned[orderID] != courierID {
				out = append(out, fmt.Sprintf("%s (courier %s, feed said %q)", orderID, courierID, seenAssigned[orderID]))
			}
		}
		return out
	}
	h.eventually(5*time.Second, "dashboard feed to show every assignment", func() bool { return len(missing()) == 0 })
	if got := h.count(`SELECT count(*) FROM assignments`); got != orders {
		t.Errorf("%d assignments, want %d", got, orders)
	}
	seenMu.Lock()
	t.Logf("dashboard feed delivered %d snapshots and showed %d assignments", snapshots, len(seenAssigned))
	seenMu.Unlock()
	_ = ids
}
