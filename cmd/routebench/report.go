package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"route/internal/config"
)

// Sample is one reading taken while a scenario runs.
type Sample struct {
	At             time.Time `json:"at"`
	T              float64   `json:"t_s"` // seconds since the first order
	QueueDepth     float64   `json:"queue_depth"`
	AwaitingMatch  int       `json:"orders_awaiting_match"` // queued or batched in Postgres
	PingsShed      float64   `json:"pings_shed_total"`
	ClaimConflicts float64   `json:"claim_conflicts_total"`
	DoubleBooked   int       `json:"double_booked"`
}

// LatencyInterval is the match latency of the orders created in one stretch
// of the load.
type LatencyInterval struct {
	FromS  float64 `json:"from_s"`
	Orders int     `json:"orders"`
	P50S   float64 `json:"p50_s"`
	P99S   float64 `json:"p99_s"`
}

// Target is one line of the spec's section 11 table, checked against what was
// measured. Pass is nil for numbers the spec asks to be reported, not judged.
type Target struct {
	Name     string `json:"name"`
	Target   string `json:"target"`
	Measured string `json:"measured"`
	Pass     *bool  `json:"pass"`
}

// Result is everything one run writes to <label>.json.
type Result struct {
	Scenario      string    `json:"scenario"`
	Label         string    `json:"label"`
	BatchWindowMs int       `json:"batch_window_ms"`
	Hardware      string    `json:"hardware"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	LoadDurationS float64   `json:"load_duration_s"`

	Sim SimSummary `json:"simulator"`

	Orders struct {
		Created   int `json:"created"`
		Assigned  int `json:"assigned"`
		Exhausted int `json:"exhausted"`
		Cancelled int `json:"cancelled"`
		Open      int `json:"still_open_at_end"`
	} `json:"orders"`
	MatchRate float64 `json:"match_rate"` // assigned / (assigned + exhausted)

	// Order created to first offer sent: the headline latency.
	MatchLatencyS struct {
		P50 float64 `json:"p50"`
		P99 float64 `json:"p99"`
		Max float64 `json:"max"`
	} `json:"match_latency_s"`
	PickupMeanM float64 `json:"mean_pickup_distance_m"` // of the offers that were accepted

	Batches struct {
		Count            int     `json:"count"`
		MaxSize          int     `json:"max_size"`
		Hungarian        int     `json:"solved_hungarian"`
		Greedy           int     `json:"solved_greedy"`
		DistanceBatched  float64 `json:"total_distance_batched_m"`
		DistanceGreedy   float64 `json:"total_distance_greedy_m"`
		DistanceRatio    float64 `json:"distance_ratio_batched_over_greedy"`
		MeanSolveMs      float64 `json:"mean_solve_ms"`
		SolverUsedGreedy float64 `json:"share_greedy_solver"`
	} `json:"batches"`

	Counters struct {
		PingsShed       float64 `json:"pings_shed"`
		PingsRejected   float64 `json:"pings_rejected"`
		ClaimConflicts  float64 `json:"claim_conflicts"`
		ClaimsRestored  float64 `json:"claims_restored"`
		PingIndexLagP99 float64 `json:"ping_index_lag_p99_s"`
	} `json:"counters"`

	// Match latency per 30 seconds of the load, by when the order was created.
	// A tail that sits in one interval points at a transient; a tail spread
	// across all of them is the steady state.
	LatencyByInterval []LatencyInterval `json:"match_latency_by_30s"`

	SurgePeak      float64   `json:"surge_peak_multiplier"`
	QueueDepthMax  float64   `json:"queue_depth_max"`
	QueueThirds    []float64 `json:"queue_depth_mean_by_third"`
	QueueStable    bool      `json:"queue_depth_stable"`
	DrainAfterSpik *float64  `json:"seconds_to_drain_after_spike"`

	DoubleBooked int      `json:"double_booked_observed"`
	Targets      []Target `json:"targets"`
	Samples      []Sample `json:"samples"`
}

// SimSummary is the part of routesim's summary the report needs.
type SimSummary struct {
	Profile    string    `json:"profile"`
	StartedAt     time.Time `json:"started_at"`
	LoadStartedAt time.Time `json:"load_started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationS     float64   `json:"duration_s"`
	Couriers      int       `json:"couriers"`
	Orders     struct {
		Sent     int64            `json:"sent"`
		Created  int64            `json:"created"`
		Dropped  int64            `json:"dropped_by_client"`
		Errors   int64            `json:"errors"`
		ByStatus map[string]int64 `json:"by_http_status"`
	} `json:"orders"`
	Latency struct {
		P50 float64 `json:"p50"`
		P99 float64 `json:"p99"`
		Max int64   `json:"max"`
	} `json:"post_orders_latency_ms_client_side"`
	Sessions struct {
		Opened      int64 `json:"opened"`
		Reconnects  int64 `json:"reconnects"`
		TokenErrors int64 `json:"token_errors"`
	} `json:"sessions"`
}

// profileInfo is the little of a scenario file the report needs: when the
// load phases ran.
type profileInfo struct {
	Phases []struct {
		Name      string  `json:"name"`
		DurationS float64 `json:"duration_s"`
	} `json:"phases"`
}

func (p profileInfo) loadDurationS() float64 {
	t := 0.0
	for _, ph := range p.Phases {
		t += ph.DurationS
	}
	return t
}

// phaseEndS is the time, in seconds from the start of the load, at which the
// named phase ends. ok is false if there is no such phase.
func (p profileInfo) phaseEndS(name string) (float64, bool) {
	t := 0.0
	for _, ph := range p.Phases {
		t += ph.DurationS
		if ph.Name == name {
			return t, true
		}
	}
	return 0, false
}

// takeSample reads the Postgres-side invariants and the in-process metrics.
func takeSample(ctx context.Context, pool *pgxpool.Pool, scrapeOK func() (queue, shed, conflicts float64)) Sample {
	s := Sample{At: time.Now()}
	s.QueueDepth, s.PingsShed, s.ClaimConflicts = scrapeOK()
	pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE status IN ('queued', 'batched')`).Scan(&s.AwaitingMatch)
	pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM (SELECT 1 FROM assignments WHERE completed_at IS NULL GROUP BY courier_id HAVING count(*) > 1) a) +
		(SELECT count(*) FROM (SELECT 1 FROM offers WHERE state = 'pending' GROUP BY courier_id HAVING count(*) > 1) b) +
		(SELECT count(*) FROM (SELECT 1 FROM (
			SELECT courier_id FROM offers WHERE state = 'pending' UNION ALL SELECT courier_id FROM assignments WHERE completed_at IS NULL
		) held GROUP BY courier_id HAVING count(*) > 1) c)`).Scan(&s.DoubleBooked)
	return s
}

// fillFromDB reads the numbers that are exact in Postgres: percentiles of
// order-to-first-offer latency, order outcomes, batch statistics.
func fillFromDB(ctx context.Context, pool *pgxpool.Pool, r *Result) error {
	rows, err := pool.Query(ctx, `SELECT status, count(*) FROM orders GROUP BY status`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var status string
		var n int
		rows.Scan(&status, &n)
		r.Orders.Created += n
		switch status {
		case "assigned", "completed":
			r.Orders.Assigned += n
		case "exhausted":
			r.Orders.Exhausted += n
		case "cancelled":
			r.Orders.Cancelled += n
		default:
			r.Orders.Open += n
		}
	}
	rows.Close()
	if t := r.Orders.Assigned + r.Orders.Exhausted; t > 0 {
		r.MatchRate = float64(r.Orders.Assigned) / float64(t)
	}

	// created -> first offer sent, per order.
	err = pool.QueryRow(ctx, `
		SELECT coalesce(percentile_cont(0.5) WITHIN GROUP (ORDER BY lat), 0),
		       coalesce(percentile_cont(0.99) WITHIN GROUP (ORDER BY lat), 0),
		       coalesce(max(lat), 0)
		FROM (SELECT extract(epoch FROM (min(f.created_at) - o.created_at)) AS lat
		      FROM orders o JOIN offers f ON f.order_id = o.id GROUP BY o.id, o.created_at) x`).
		Scan(&r.MatchLatencyS.P50, &r.MatchLatencyS.P99, &r.MatchLatencyS.Max)
	if err != nil {
		return err
	}
	if !r.Sim.LoadStartedAt.IsZero() {
		rows, err := pool.Query(ctx, `
			SELECT floor(extract(epoch FROM (created_at - $1)) / 30)::int AS b, count(*),
			       percentile_cont(0.5) WITHIN GROUP (ORDER BY lat),
			       percentile_cont(0.99) WITHIN GROUP (ORDER BY lat)
			FROM (SELECT o.created_at, extract(epoch FROM (min(f.created_at) - o.created_at)) AS lat
			      FROM orders o JOIN offers f ON f.order_id = o.id
			      WHERE o.created_at >= $1 GROUP BY o.id, o.created_at) x
			GROUP BY b ORDER BY b`, r.Sim.LoadStartedAt)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b, n int
			var p50, p99 float64
			if err := rows.Scan(&b, &n, &p50, &p99); err == nil {
				r.LatencyByInterval = append(r.LatencyByInterval, LatencyInterval{float64(b * 30), n, p50, p99})
			}
		}
		rows.Close()
	}
	pool.QueryRow(ctx, `SELECT coalesce(avg(f.pickup_m), 0) FROM assignments a JOIN offers f ON f.id = a.offer_id`).Scan(&r.PickupMeanM)

	var hungarian, greedy int
	err = pool.QueryRow(ctx, `
		SELECT count(*), coalesce(max(order_count), 0),
		       count(*) FILTER (WHERE solver = 'hungarian'), count(*) FILTER (WHERE solver = 'greedy'),
		       coalesce(sum(total_distance_batched), 0), coalesce(sum(total_distance_greedy), 0),
		       coalesce(avg(solve_duration_ms), 0)
		FROM batches`).
		Scan(&r.Batches.Count, &r.Batches.MaxSize, &hungarian, &greedy,
			&r.Batches.DistanceBatched, &r.Batches.DistanceGreedy, &r.Batches.MeanSolveMs)
	if err != nil {
		return err
	}
	r.Batches.Hungarian, r.Batches.Greedy = hungarian, greedy
	if r.Batches.DistanceGreedy > 0 {
		r.Batches.DistanceRatio = r.Batches.DistanceBatched / r.Batches.DistanceGreedy
	}
	if r.Batches.Count > 0 {
		r.Batches.SolverUsedGreedy = float64(greedy) / float64(r.Batches.Count)
	}
	pool.QueryRow(ctx, `SELECT coalesce(max(multiplier), 1) FROM surge_snapshots`).Scan(&r.SurgePeak)
	return nil
}

// queueStability judges whether the queue stayed bounded. It splits the load
// period into thirds and compares the mean depth of the last third with the
// middle one: stable if the last third is no more than twice the middle third
// (or small in absolute terms, under 100). Needs at least 6 samples.
func queueStability(samples []Sample, loadDurationS float64) (thirds []float64, stable bool) {
	thirds = make([]float64, 3)
	counts := make([]int, 3)
	for _, s := range samples {
		if s.T < 0 || s.T >= loadDurationS {
			continue
		}
		i := int(3 * s.T / loadDurationS)
		thirds[i] += s.QueueDepth
		counts[i]++
	}
	for i := range thirds {
		if counts[i] == 0 {
			return thirds, false
		}
		thirds[i] /= float64(counts[i])
	}
	return thirds, thirds[2] <= math.Max(100, 2*thirds[1])
}

// secondsToDrain finds how long after spikeEndS the number of orders awaiting
// a match first fell to threshold or below.
func secondsToDrain(samples []Sample, spikeEndS float64, threshold int) (float64, bool) {
	for _, s := range samples {
		if s.T >= spikeEndS && s.AwaitingMatch <= threshold {
			return s.T - spikeEndS, true
		}
	}
	return 0, false
}

func ptr(b bool) *bool { return &b }

// targetsFor holds the section 11 table. Numbers the spec asks to be
// published rather than judged have a nil Pass.
func targetsFor(scenario string, r *Result) []Target {
	var ts []Target
	add := func(name, target, measured string, pass *bool) {
		ts = append(ts, Target{name, target, measured, pass})
	}
	matchLatency := func(limit float64) {
		add("match latency p99", fmt.Sprintf("< %.0f s", limit), fmt.Sprintf("%.2f s (p50 %.2f s)", r.MatchLatencyS.P99, r.MatchLatencyS.P50), ptr(r.MatchLatencyS.P99 < limit))
	}
	matchRate := func(limit float64) {
		add("match rate", fmt.Sprintf("> %.0f%%", limit*100), fmt.Sprintf("%.2f%% (%d assigned, %d exhausted, %d still open)", r.MatchRate*100, r.Orders.Assigned, r.Orders.Exhausted, r.Orders.Open), ptr(r.MatchRate > limit))
	}

	switch scenario {
	case "baseline":
		matchLatency(3)
		matchRate(0.98)
		add("pings shed", "0", fmt.Sprintf("%.0f", r.Counters.PingsShed), ptr(r.Counters.PingsShed == 0))
	case "throughput":
		matchLatency(4)
		matchRate(0.95)
		add("queue depth stable", "last third of load <= 2x the middle third (or under 100)",
			fmt.Sprintf("thirds %s, max %.0f", fmtFloats(r.QueueThirds), r.QueueDepthMax), ptr(r.QueueStable))
	case "ping-flood":
		add("ping index lag p99", "< 1 s", fmt.Sprintf("%.3f s", r.Counters.PingIndexLagP99), ptr(r.Counters.PingIndexLagP99 < 1))
		add("pings shed", "acceptable, reported", fmt.Sprintf("%.0f", r.Counters.PingsShed), nil)
		matchLatency(3)
	case "hotspot-spike":
		add("batch size cap holds", fmt.Sprintf("max batch <= %d orders", config.MaxBatchSize), fmt.Sprintf("%d", r.Batches.MaxSize), ptr(r.Batches.MaxSize <= config.MaxBatchSize))
		if r.DrainAfterSpik != nil {
			add("queue drains after the spike", "within 60 s", fmt.Sprintf("%.0f s", *r.DrainAfterSpik), ptr(*r.DrainAfterSpik <= 60))
		} else {
			add("queue drains after the spike", "within 60 s", "did not drain before the run ended", ptr(false))
		}
		add("surge reaches the maximum", fmt.Sprintf("%.1fx", surgeMax), fmt.Sprintf("%.1fx", r.SurgePeak), ptr(r.SurgePeak >= surgeMax))
	case "contention":
		add("exhausted orders", "non-zero, reported honestly",
			fmt.Sprintf("%d of %d orders (%.1f%%)", r.Orders.Exhausted, r.Orders.Created, 100*float64(r.Orders.Exhausted)/math.Max(1, float64(r.Orders.Created))), nil)
		add("claim conflicts", "counted", fmt.Sprintf("%.0f", r.Counters.ClaimConflicts), nil)
	case "solver-comparison":
		add("mean pickup distance", "reported", fmt.Sprintf("%.0f m", r.PickupMeanM), nil)
		add("match latency", "reported", fmt.Sprintf("p50 %.2f s, p99 %.2f s", r.MatchLatencyS.P50, r.MatchLatencyS.P99), nil)
	}
	add("no double booking", "0 observed", fmt.Sprintf("%d", r.DoubleBooked), ptr(r.DoubleBooked == 0))
	if scenario == "contention" {
		// The scenario is under-supplied on purpose; a non-zero exhausted
		// count is the expected result, so also show whether it was zero.
		ts[0].Pass = ptr(r.Orders.Exhausted > 0)
	}
	return ts
}

// surgeMax is the top step of the surge multiplier.
const surgeMax = 3.0

func fmtFloats(xs []float64) string {
	s := "["
	for i, x := range xs {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%.0f", x)
	}
	return s + "]"
}
