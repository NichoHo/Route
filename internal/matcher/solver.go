package matcher

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"route/internal/config"
	"route/internal/index"
	"route/internal/metrics"
	"route/internal/offers"
)

const infiniteCost = 999999999.0

// maxMatchPasses is how many times SolveBatch solves the orders that lost out.
const maxMatchPasses = 3

// phase times one step of a batch; the histogram shows where a slow batch
// spends its time.
func phase(name string) func() {
	t := time.Now()
	return func() { metrics.BatchPhaseSeconds.WithLabelValues(name).Observe(time.Since(t).Seconds()) }
}

func (b *Batcher) SolveBatch(ctx context.Context, orderIDs []string, windowOpenAt time.Time) {
	tracer := otel.Tracer("matcher")
	ctx, span := tracer.Start(ctx, "SolveBatch")
	defer span.End()

	start := time.Now()
	if windowOpenAt.IsZero() {
		windowOpenAt = start
	}

	// Fetch order details
	// Remove duplicates from orderIDs (could happen if re-triggered)
	uniqueMap := make(map[string]bool)
	var uniqueIDs []string
	for _, id := range orderIDs {
		if !uniqueMap[id] {
			uniqueMap[id] = true
			uniqueIDs = append(uniqueIDs, id)
		}
	}

	orders := make([]Order, 0, len(uniqueIDs))
	excludedLists := make([][]string, 0, len(uniqueIDs))

	// Claim the orders atomically: only rows still 'queued' flip to 'batched',
	// so a concurrent cancel or sweep can't be overwritten by this batch.
	endClaim := phase("claim_orders")
	rows, err := b.pool.Query(ctx, `
		UPDATE orders SET status = 'batched', updated_at = NOW()
		WHERE id = ANY($1) AND status = 'queued'
		RETURNING id, ST_Y(pickup::geometry), ST_X(pickup::geometry), created_at, excluded_couriers, priority
	`, uniqueIDs)
	if err != nil {
		log.Printf("failed to claim orders for batch: %v", err)
		return
	}
	for rows.Next() {
		var o Order
		var excluded []string
		if err := rows.Scan(&o.ID, &o.PickupLat, &o.PickupLng, &o.CreatedAt, &excluded, &o.Priority); err == nil {
			orders = append(orders, o)
			if excluded == nil {
				excluded = []string{}
			}
			excludedLists = append(excludedLists, excluded)
		}
	}
	rows.Close() // don't hold this connection through the offer transactions below
	endClaim()

	if len(orders) == 0 {
		return
	}

	// Pass 1 solves every order with small candidate lists. Orders that lose
	// out because there are more orders than couriers in reach are solved again
	// at once, against the next-nearest free couriers (pass 1's claims already
	// hide the ones it took), instead of waiting a whole window. Forcing every
	// order to find a batch-sized candidate list up front made the cost matrix
	// thousands of columns wide and slower overall.
	pending := make([]int, len(orders))
	for i := range pending {
		pending[i] = i
	}
	for pass := 1; len(pending) > 0 && pass <= maxMatchPasses; pass++ {
		passOrders := make([]Order, len(pending))
		passExcluded := make([][]string, len(pending))
		for k, i := range pending {
			passOrders[k], passExcluded[k] = orders[i], excludedLists[i]
		}
		minCandidates := 10
		if pass > 1 && len(passOrders) > minCandidates {
			minCandidates = len(passOrders)
		}
		passLabel := strconv.Itoa(pass)
		metrics.MatchPassOrdersTotal.WithLabelValues(passLabel).Add(float64(len(passOrders)))
		leftover := b.matchPass(ctx, pass, passOrders, passExcluded, windowOpenAt, start, minCandidates)
		next := make([]int, len(leftover))
		for k, j := range leftover {
			next[k] = pending[j]
		}
		metrics.MatchPassLeftoverTotal.WithLabelValues(passLabel).Add(float64(len(next)))
		noProgress := len(next) == len(pending)
		pending = next
		if noProgress {
			break // a pass that served nobody would only repeat itself
		}
	}
	// Still unserved after every pass: back to the queue for the next window.
	for _, i := range pending {
		b.requeue(ctx, orders[i].ID, false)
		b.retrigger(orders[i].ID)
	}
}

// matchPass finds candidates, solves and makes offers for one set of claimed
// ('batched') orders. pass is 1 for the first attempt. It returns the indexes of the orders that lost out
// because no courier was left for them. Every other order has been offered or
// put back in the queue.
func (b *Batcher) matchPass(ctx context.Context, pass int, orders []Order, excludedLists [][]string, windowOpenAt, start time.Time, minCandidates int) (leftover []int) {
	// Gather candidates across all orders using the spatial index.
	endCandidates := phase("candidates")
	courierMap := make(map[string]index.CourierState)
	perOrder := config.CandidatesPerOrder
	if pass > 1 && len(orders) > 50 {
		perOrder = len(orders)
	} else if pass > 1 {
		perOrder = 50
	}
	points := make([]index.Point, len(orders))
	for i, o := range orders {
		points[i] = index.Point{Lat: o.PickupLat, Lng: o.PickupLng}
	}
	found, err := b.store.FindCandidatesBatch(ctx, points, perOrder, minCandidates)
	if err != nil {
		// Redis is unreachable. No candidates means the orders go back to the
		// queue below.
		log.Printf("failed to find candidates for the batch: %v", err)
		found = nil
	}
	for _, candidates := range found {
		for _, c := range candidates {
			courierMap[c.ID] = c
		}
	}
	endCandidates()
	if len(courierMap) == 0 {
		if pass > 1 {
			// These orders already lost out to scarcity in an earlier pass and
			// the free couriers are now all taken. Return them all: they are
			// retried next window.
			all := make([]int, len(orders))
			for i := range all {
				all[i] = i
			}
			return all
		}
		// No couriers in reach at all. Put the orders back to 'queued' so the
		// sweep re-triggers them once RequeueStaleAfter has passed; retrying
		// every window would only spin.
		for _, o := range orders {
			b.requeue(ctx, o.ID, false)
		}
		return nil
	}

	allCouriers := make([]index.CourierState, 0, len(courierMap))
	for _, c := range courierMap {
		allCouriers = append(allCouriers, c)
	}
	// Deterministic column order: map iteration order is random, and a
	// random tie-break makes the batched-vs-greedy comparison and any
	// debugging non-reproducible from one run to the next.
	sort.Slice(allCouriers, func(i, j int) bool { return allCouriers[i].ID < allCouriers[j].ID })

	// Cost Matrix
	N := len(orders)
	M := len(allCouriers)
	now := time.Now()
	endRates := phase("accept_rates")
	acceptRates := b.acceptRates(ctx, allCouriers)
	endRates()
	endMatrix := phase("matrix")
	costMatrix := make([][]float64, N)
	for i := 0; i < N; i++ {
		costMatrix[i] = make([]float64, M)

		excludedMap := make(map[string]bool)
		for _, e := range excludedLists[i] {
			excludedMap[e] = true
		}

		pressure := ZonePressure(b.surge.GetMultiplier(orders[i].PickupLat, orders[i].PickupLng))

		for j := 0; j < M; j++ {
			c := allCouriers[j]
			dist := Haversine(orders[i].PickupLat, orders[i].PickupLng, c.Lat, c.Lng)
			if excludedMap[c.ID] || dist > config.MaxCandidateRadiusMeters {
				costMatrix[i][j] = infiniteCost
				continue
			}
			costMatrix[i][j] = costForDistance(b.weights, orders[i], dist, ScoreInputs{
				Now:          now,
				AcceptRate:   acceptRates[c.ID],
				ZonePressure: pressure,
			})
		}
	}
	endMatrix()

	endSolve := phase("solve")
	var assignment []int
	solverUsed := "greedy"

	// Solve using greedy to get baseline cost
	greedyAssignment := solveGreedy(costMatrix)
	greedyCost, greedyMatched := calcTotalCost(costMatrix, greedyAssignment)

	if HungarianWork(N, M) <= config.HungarianMaxWork {
		assignment = SolveHungarian(costMatrix)
		solverUsed = "hungarian"
	} else {
		assignment = greedyAssignment
	}

	batchedCost, batchedMatched := calcTotalCost(costMatrix, assignment)
	batchedDist := pickupDistance(orders, allCouriers, costMatrix, assignment)
	greedyDist := pickupDistance(orders, allCouriers, costMatrix, greedyAssignment)
	endSolve()

	metrics.BatchSolveSeconds.WithLabelValues(solverUsed).Observe(time.Since(start).Seconds())
	// The live comparison is in pickup distance, which is what batching is
	// meant to shorten. The solvers minimise cost, which also holds wait,
	// accept-rate and priority terms, so the ratio can exceed 1. Only compare
	// when both matched the same number of orders; otherwise a smaller total
	// may just mean fewer matches.
	if greedyDist > 0 && batchedMatched == greedyMatched {
		metrics.BatchedVsGreedyRatio.Set(batchedDist / greedyDist)
	}

	trace.SpanFromContext(ctx).SetAttributes(
		attribute.String("solver.type", solverUsed),
		attribute.Int("batch.size", N),
		attribute.Int("courier.count", M),
	)

	// Record the batch off the critical path: it is bookkeeping, and under
	// load it cost 64ms of every batch. The goroutine has its own deadline and
	// Stop waits for it.
	batchID := uuid.New().String()
	solveDuration := time.Since(start)
	b.recording.Add(1)
	go func() {
		defer b.recording.Done()
		endRecord := phase("record")
		defer endRecord()
		rctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		payload, _ := json.Marshal(map[string]interface{}{
			"id":               batchID,
			"size":             N,
			"solver":           solverUsed,
			"batched_distance": batchedDist,
			"greedy_distance":  greedyDist,
			"batched_cost":     batchedCost,
			"greedy_cost":      greedyCost,
		})
		if b.outbox != nil {
			b.outbox.Publish(rctx, "batch.solved", payload)
		}
		_, err := b.pool.Exec(rctx, `
			INSERT INTO batches (id, window_open_at, window_close_at, order_count, courier_count, solver, total_cost_batched, total_cost_greedy, total_distance_batched, total_distance_greedy, solve_duration_ms)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		`, batchID, windowOpenAt, start, N, M, solverUsed, batchedCost, greedyCost, batchedDist, greedyDist, solveDuration.Milliseconds())
		if err != nil {
			log.Printf("failed to record batch %s: %v", batchID, err)
		}
	}()

	// Execute the assignments, a few at a time. Orders that lost out (more
	// orders than couriers) are returned for another pass; orders with no
	// reachable courier at all go back to the queue for the sweep.
	endOffers := phase("offers")
	var leftMu sync.Mutex
	parallel(N, config.MatchConcurrency, func(i int) {
		col := assignment[i]
		if col == -1 {
			leftMu.Lock()
			leftover = append(leftover, i)
			leftMu.Unlock()
			return
		}
		if costMatrix[i][col] >= infiniteCost {
			// Every courier in the pool is excluded or out of range for this
			// order. Retrying next window would spin; leave it to the sweep.
			b.requeue(ctx, orders[i].ID, false)
			return
		}

		c := allCouriers[col]
		o := orders[i]
		cCost := costMatrix[i][col]

		offerID := uuid.New().String()
		ok, err := b.claims.AcquireOfferClaim(ctx, c.ID, offerID, config.OfferClaimTTL)
		if err == nil && ok {
			err = b.lifecycle.CreateOffer(ctx, offerID, o.ID, c.ID, cCost, Haversine(o.PickupLat, o.PickupLng, c.Lat, c.Lng))
			if err != nil {
				b.claims.ReleaseClaim(ctx, c.ID, offerID)
				if errors.Is(err, offers.ErrCourierBusy) {
					// Postgres says this courier is committed but Redis let
					// the claim through: Redis lost it. Put the real claim
					// back so the courier stops being a candidate.
					metrics.ClaimConflictsTotal.Inc()
					b.lifecycle.RepairCourierClaim(ctx, c.ID)
					b.requeue(ctx, o.ID, false)
					b.retrigger(o.ID)
				} else {
					log.Printf("failed to create offer for %s: %v", o.ID, err)
					b.requeue(ctx, o.ID, false)
				}
			} else {
				metrics.MatchRate.Inc()
				metrics.MatchLatencySeconds.Observe(time.Since(o.CreatedAt).Seconds())
			}
		} else {
			metrics.ClaimConflictsTotal.Inc()
			b.requeue(ctx, o.ID, true) // lost the claim race: boost priority (ROUTE_SPEC.md 6.3)
			b.retrigger(o.ID)
		}
	})
	endOffers()
	sort.Ints(leftover) // parallel appends arrive in any order
	return leftover
}

// requeue returns a batched order to 'queued'. Guarded on status so an order
// cancelled or exhausted mid-batch is left alone. boost raises its priority
// by one level, for an order that lost a claim race.
func (b *Batcher) requeue(ctx context.Context, orderID string, boost bool) {
	b.pool.Exec(ctx, `
		UPDATE orders SET status = 'queued', updated_at = NOW(),
		       priority = CASE WHEN $2 THEN LEAST(priority + 1, $3) ELSE priority END
		WHERE id = $1 AND status = 'batched'`, orderID, boost, config.MaxPriority)
}

// parallel runs fn for every index in [0, n), at most workers at a time, and
// returns when all have finished.
func parallel(n, workers int, fn func(i int)) {
	if workers > n {
		workers = n
	}
	next := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				fn(i)
			}
		}()
	}
	for i := 0; i < n; i++ {
		next <- i
	}
	close(next)
	wg.Wait()
}

// retrigger puts an order back in the batcher's queue after one window.
func (b *Batcher) retrigger(orderID string) {
	time.AfterFunc(b.windowMs, func() {
		if b.ctx.Err() == nil {
			b.Trigger(orderID)
		}
	})
}

// rateCacheTTL is how long a courier's accept rate is reused. Rates are built
// from an hour of offers and move slowly, so a stale one costs little, and the
// lookup was 71ms of every batch under load.
const rateCacheTTL = 30 * time.Second

type rateEntry struct {
	rate float64
	at   time.Time
}

// acceptRates returns each courier's smoothed accept rate from their offers
// in the last AcceptRateWindow. Offers on orders that were later cancelled do
// not count against the courier. Rates are cached for rateCacheTTL. If the
// lookup fails, those couriers are scored at the prior rather than failing the
// batch.
func (b *Batcher) acceptRates(ctx context.Context, couriers []index.CourierState) map[string]float64 {
	now := time.Now()
	rates := make(map[string]float64, len(couriers))
	var missing []string
	b.rateMu.Lock()
	for _, c := range couriers {
		if e, ok := b.rates[c.ID]; ok && now.Sub(e.at) < rateCacheTTL {
			rates[c.ID] = e.rate
		} else {
			missing = append(missing, c.ID)
		}
	}
	b.rateMu.Unlock()
	if len(missing) == 0 {
		return rates
	}

	fetched := make(map[string]float64, len(missing))
	rows, err := b.pool.Query(ctx, `
		SELECT f.courier_id,
		       count(*) FILTER (WHERE f.state = 'accepted'),
		       count(*) FILTER (WHERE f.state IN ('declined', 'expired'))
		FROM offers f JOIN orders o ON o.id = f.order_id
		WHERE f.courier_id = ANY($1) AND f.state <> 'pending'
		  AND f.resolved_at > NOW() - ($2 * INTERVAL '1 second')
		  AND o.status <> 'cancelled'
		GROUP BY f.courier_id`, missing, config.AcceptRateWindow.Seconds())
	failed := err != nil
	if failed {
		log.Printf("accept rate lookup failed, scoring at the prior: %v", err)
	} else {
		for rows.Next() {
			var id string
			var accepted, rejected int
			if err := rows.Scan(&id, &accepted, &rejected); err == nil {
				fetched[id] = SmoothedAcceptRate(accepted, rejected)
			}
		}
		rows.Close()
	}

	b.rateMu.Lock()
	defer b.rateMu.Unlock()
	if len(b.rates) > 50000 { // bound the cache: drop it rather than track ages
		b.rates = make(map[string]rateEntry)
	}
	for _, id := range missing {
		r, ok := fetched[id]
		if !ok {
			r = SmoothedAcceptRate(0, 0) // no history in the window
		}
		rates[id] = r
		if !failed { // don't cache a guess made because the lookup failed
			b.rates[id] = rateEntry{r, now}
		}
	}
	return rates
}

// pickupDistance sums the courier-to-pickup distance of the matched pairs.
func pickupDistance(orders []Order, couriers []index.CourierState, costMatrix [][]float64, assignment []int) float64 {
	total := 0.0
	for i, col := range assignment {
		if col != -1 && costMatrix[i][col] < infiniteCost {
			total += Haversine(orders[i].PickupLat, orders[i].PickupLng, couriers[col].Lat, couriers[col].Lng)
		}
	}
	return total
}

// solveGreedy is the baseline the batched-vs-greedy comparison is measured
// against: take the globally cheapest remaining (order, courier) pair until
// no finite pair is left.
func solveGreedy(costMatrix [][]float64) []int {
	N := len(costMatrix)
	if N == 0 {
		return nil
	}
	M := len(costMatrix[0])

	type pair struct {
		i, j int
		cost float64
	}
	pairs := make([]pair, 0, N*M)
	for i := 0; i < N; i++ {
		for j := 0; j < M; j++ {
			if costMatrix[i][j] < infiniteCost {
				pairs = append(pairs, pair{i, j, costMatrix[i][j]})
			}
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int {
		if c := cmp.Compare(a.cost, b.cost); c != 0 {
			return c
		}
		if c := cmp.Compare(a.i, b.i); c != 0 {
			return c
		}
		return cmp.Compare(a.j, b.j)
	})

	assignment := make([]int, N)
	for i := range assignment {
		assignment[i] = -1
	}
	usedCol := make([]bool, M)
	for _, p := range pairs {
		if assignment[p.i] == -1 && !usedCol[p.j] {
			assignment[p.i] = p.j
			usedCol[p.j] = true
		}
	}
	return assignment
}

// calcTotalCost sums the finite-cost pairs and returns how many were matched.
func calcTotalCost(costMatrix [][]float64, assignment []int) (float64, int) {
	total, matched := 0.0, 0
	for i, col := range assignment {
		if col != -1 && costMatrix[i][col] < infiniteCost {
			total += costMatrix[i][col]
			matched++
		}
	}
	return total, matched
}
