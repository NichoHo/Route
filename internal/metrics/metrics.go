package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

var (
	PingsShedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "route_pings_shed_total",
		Help: "Total number of pings shed due to backpressure",
	})

	OutOfOrderPingsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "route_out_of_order_pings_total",
		Help: "Total number of pings dropped because they arrived out of order (lower seq)",
	})

	PingsRejectedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_pings_rejected_total",
		Help: "Pings rejected by validation, by reason",
	}, []string{"reason"})

	PingIndexLagSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "route_ping_index_lag_seconds",
		Help:    "Time from a ping arriving to its position being written to the Redis index",
		Buckets: []float64{0.001, 0.005, 0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5},
	})

	BatchSize = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "route_batch_size",
		Help:    "Number of orders in a matcher batch",
		Buckets: []float64{1, 5, 10, 50, 100, 200, 500},
	})

	BatchSolveSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "route_batch_solve_seconds",
		Help:    "Time taken to solve a matcher batch",
		Buckets: prometheus.DefBuckets,
	}, []string{"solver"})

	BatchPhaseSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "route_batch_phase_seconds",
		Help:    "Time one batch spends in each phase: claim_orders, candidates, accept_rates, matrix, solve, record, offers",
		Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
	}, []string{"phase"})

	CandidateSearchesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_candidate_searches_total",
		Help: "GEOSEARCH calls made to find couriers, by step of the radius ladder (0 is the smallest radius). Searching is the matcher's main cost on Redis.",
	}, []string{"step"})

	MatchPassOrdersTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_match_pass_orders_total",
		Help: "Orders entering each matching pass. Pass 2 and 3 handle orders that lost out in the pass before.",
	}, []string{"pass"})

	MatchPassLeftoverTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_match_pass_leftover_total",
		Help: "Orders that lost out in each matching pass because no courier was left for them.",
	}, []string{"pass"})

	BatchedVsGreedyRatio = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "route_batched_vs_greedy_ratio",
		Help: "Ratio of total courier-to-pickup distance (batched / greedy) for the last batch. Below 1 means batching shortened pickups; it can exceed 1 when other cost terms (wait, accept rate, priority) outweigh distance.",
	})

	MatchLatencySeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "route_match_latency_seconds",
		Help:    "Time taken from order creation to successful match/offer",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
	})

	MatchRate = promauto.NewCounter(prometheus.CounterOpts{
		Name: "route_matches_total",
		Help: "Total number of successful matches",
	})

	QueueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "route_queue_depth",
		Help: "Current number of unassigned queued orders",
	})

	TriggersDroppedTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "route_triggers_dropped_total",
		Help: "Matcher wake-ups dropped because its queue was full. The orders are safe in Postgres and the sweep retries them, so this measures overload, not loss.",
	})

	ClaimConflictsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "route_claim_conflicts_total",
		Help: "Total number of Redis claim conflicts during matching",
	})

	ClaimsReconciledTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_claims_reconciled_total",
		Help: "Courier claims found out of line with Postgres by reconciliation: restored (missing or stale) or mismatch (held under another value)",
	}, []string{"result"})

	OfferOutcomeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "route_offer_outcome_total",
		Help: "Outcomes of offers to couriers",
	}, []string{"outcome"}) // "accepted", "declined", "expired", "failed"
)

func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		return 0
	}
	return m.GetCounter().GetValue()
}

// MatchesTotal returns the current value of route_matches_total, so the
// dashboard can show it without scraping /metrics.
func MatchesTotal() float64 { return counterValue(MatchRate) }

// ClaimConflicts returns the current value of route_claim_conflicts_total.
func ClaimConflicts() float64 { return counterValue(ClaimConflictsTotal) }
