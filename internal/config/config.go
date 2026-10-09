package config

import (
	"os"
	"strconv"
	"time"
)

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if val, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return fallback
}

// MaxPriority is the highest order priority. Keep in sync with the CHECK on
// orders.priority in deploy/postgres/init.sql.
const MaxPriority = 9

const (
	MaxAttempts   = 3
	OfferDeadline = 15 * time.Second

	// OfferClaimTTL is how long a courier stays claimed while an offer is
	// pending. It must exceed OfferDeadline so the claim can't expire out
	// from under a still-pending offer.
	OfferClaimTTL = OfferDeadline + 5*time.Second

	// AssignmentClaimTTL is how long a courier stays claimed after
	// accepting, covering the simulated delivery. It's a backstop, not the
	// primary release path: Complete() releases the claim explicitly when
	// the courier reports completion.
	AssignmentClaimTTL = time.Hour

	MaxBatchSize = 100

	// CandidatesPerOrder is how many of the nearest free couriers each order
	// asks for in the first pass. Small on purpose: the Redis reply and the cost
	// matrix both grow with it. Orders that lose out because their hotspot's
	// nearest couriers ran short are solved again with a larger search, sized
	// to how many orders are left.
	CandidatesPerOrder = 20

	// MaxConcurrentBatches is how many batches may be solving at once. Two
	// gave no more throughput than one in the load test (the cost is shared
	// Redis and Postgres work, not waiting), and two batches can pick the same
	// courier, so the default is one. Raise it if a deployment is bound by
	// latency rather than CPU.
	MaxConcurrentBatches = 1

	// MatchConcurrency bounds how many candidate lookups and offer creations
	// one batch runs at once. They are independent: claims are atomic, so
	// concurrent offers cannot double-book a courier. Run one at a time, a
	// 100-order batch took 790ms in the load test, which capped the matcher at
	// about 125 orders a second.
	MatchConcurrency = 32

	// HungarianMaxWork caps the Hungarian solver's estimated work
	// (min(orders,couriers)^2 * max(orders,couriers), see
	// matcher.HungarianWork); larger batches fall back to greedy. Benchmarked
	// at ~6ms for 100 orders x 5000 couriers (5e7), so 1e8 is roughly 12ms
	// and covers every batch the default MaxBatchSize can produce.
	HungarianMaxWork = 100_000_000

	// SweepInterval is how often the background sweep runs: expiring
	// timed-out offers and re-triggering orders stuck in queued/batched.
	SweepInterval = 2 * time.Second

	// RequeueStaleAfter is how long an order can sit in queued/batched
	// before the sweep re-triggers it.
	RequeueStaleAfter = 5 * time.Second

	// MaxQueueAge is how long an order can go unmatched before the sweep
	// gives up on it, so an order with no reachable courier doesn't sit
	// forever getting silently re-triggered every window.
	MaxQueueAge = 2 * time.Minute

	StaleCourierTimeout = 15 * time.Second

	// ReconcileInterval is how often courier claims in Redis are checked
	// against Postgres (ROUTE_SPEC.md section 6.6). It also runs once at start.
	ReconcileInterval = 30 * time.Second

	// BroadcastInterval is the dashboard snapshot rate (4 Hz).
	BroadcastInterval = 250 * time.Millisecond

	// MaxWSClients caps concurrent dashboard WebSocket connections.
	MaxWSClients = 100

	// MaxCandidateRadiusKm is the outermost step of the candidate search
	// radius ladder (see index.FindCandidates) and the distance beyond
	// which the solver treats a courier as unreachable for an order, since
	// the candidate pool is a union across every order in the batch and a
	// courier close to one order's pickup can still be too far from
	// another's.
	MaxCandidateRadiusKm     = 8.0
	MaxCandidateRadiusMeters = MaxCandidateRadiusKm * 1000
)

func getEnvFloat(key string, fallback float64) float64 {
	if val, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(val, 64); err == nil && f > 0 {
			return f
		}
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// PostgresMaxConns is the size of the connection pool. It must comfortably
// exceed MatchConcurrency, or concurrent offer creations queue for a connection.
var PostgresMaxConns = getEnvInt("POSTGRES_MAX_CONNS", 64)

// Abuse limits. Defaults sit above the ROUTE_SPEC.md section 11 load
// scenarios (500 orders/s, 5000 couriers) so load tests run unthrottled;
// lower them for a public deployment.
var (
	// OrderRatePerIP and OrderRatePerToken are sustained requests/second
	// allowed on the mutating HTTP endpoints.
	OrderRatePerIP    = getEnvFloat("ORDER_RATE_PER_IP", 1000)
	OrderRatePerToken = getEnvFloat("ORDER_RATE_PER_TOKEN", 1000)

	// MaxCourierSessions caps concurrent gRPC courier sessions: the largest
	// simulator (5000) plus a margin.
	MaxCourierSessions = getEnvInt("MAX_COURIER_SESSIONS", 5500)
)

func getEnvNonNegFloat(key string, fallback float64) float64 {
	if val, ok := os.LookupEnv(key); ok {
		if f, err := strconv.ParseFloat(val, 64); err == nil && f >= 0 {
			return f
		}
	}
	return fallback
}

// ScoringWeights are the weights of the matcher's cost function (ROUTE_SPEC.md
// section 6.4; reasoning in docs/decisions/010-scoring-weights.md). Every
// weight is in meters of pickup distance, so "Wait: 10" means one second of
// waiting is worth ten meters. All weights are non-negative; zero turns a term
// off.
type ScoringWeights struct {
	Dist     float64       // per meter of courier-to-pickup distance
	Wait     float64       // per second an order has left before WaitCap
	WaitCap  time.Duration // age after which waiting stops earning priority
	Accept   float64       // for a courier who never accepts
	Load     float64       // for an order in a zone with no demand pressure
	Priority float64       // per priority level below MaxPriority
}

// Scoring is read from the environment at start. The defaults are heuristics,
// not calibrated values: there is no road network to calibrate against.
var Scoring = ScoringWeights{
	Dist:     getEnvNonNegFloat("SCORE_W_DIST", 1),
	Wait:     getEnvNonNegFloat("SCORE_W_WAIT", 10),
	WaitCap:  getEnvDuration("SCORE_WAIT_CAP", 60*time.Second),
	Accept:   getEnvNonNegFloat("SCORE_W_ACCEPT", 500),
	Load:     getEnvNonNegFloat("SCORE_W_LOAD", 300),
	Priority: getEnvNonNegFloat("SCORE_W_PRIORITY", 100),
}

// A courier's accept rate is their offers in the last AcceptRateWindow,
// smoothed toward AcceptRatePrior as if they had AcceptRatePriorWeight extra
// offers at that rate, so a courier with no history is scored as average.
const (
	AcceptRateWindow      = time.Hour
	AcceptRatePrior       = 0.85
	AcceptRatePriorWeight = 5.0
)

// BatchWindow is env-overridable so the solver-comparison load test
// (ROUTE_SPEC.md section 11) can vary it without a rebuild.
var BatchWindow = getEnvDuration("BATCH_WINDOW", 2*time.Second)

var (
	// Bounding box for Central Jakarta
	JakartaMinLat    = -6.23
	JakartaMaxLat    = -6.17
	JakartaMinLng    = 106.80
	JakartaMaxLng    = 106.85
	JakartaCenterLat = (JakartaMinLat + JakartaMaxLat) / 2
	JakartaCenterLng = (JakartaMinLng + JakartaMaxLng) / 2
)
