// routebench watches a load-test run and writes its report.
//
// It is started before the simulator. While the scenario runs it samples
// routed's metrics and Postgres every few seconds. When routesim writes its
// summary file, the scenario is over: routebench then reads the exact numbers
// from Postgres, checks them against the targets in ROUTE_SPEC.md section 11,
// and writes <label>.json and <label>.svg into the results directory.
//
//	routebench            watch one run (configured by environment)
//	routebench compare    combine the solver-comparison runs into one chart
//	routebench summary    write SUMMARY.md from every result file
//
// A missed target is reported, not hidden: the exit code is 0 either way.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "compare" || os.Args[1] == "summary") {
		dir := envOr("RESULTS_DIR", "loadtest/results")
		if len(os.Args) > 2 {
			dir = os.Args[2]
		}
		run := runCompare
		if os.Args[1] == "summary" {
			run = runSummary
		}
		if err := run(dir); err != nil {
			log.Fatal(err)
		}
		return
	}
	if err := runWatch(); err != nil {
		log.Fatal(err)
	}
}

func runWatch() error {
	scenario := os.Getenv("BENCH_SCENARIO")
	if scenario == "" {
		return fmt.Errorf("BENCH_SCENARIO must be set")
	}
	label := envOr("BENCH_LABEL", scenario)
	resultsDir := envOr("RESULTS_DIR", "/results")
	metricsURL := envOr("METRICS_URL", "http://routed:9100/metrics")
	every, err := time.ParseDuration(envOr("SAMPLE_EVERY", "5s"))
	if err != nil {
		return err
	}
	windowMs, _ := strconv.Atoi(envOr("BENCH_WINDOW_MS", "2000"))

	var prof profileInfo
	if raw, err := os.ReadFile(filepath.Join(envOr("SCENARIOS_DIR", "/scenarios"), scenario+".json")); err == nil {
		json.Unmarshal(raw, &prof)
	} else {
		log.Printf("no scenario file for %q (%v): drain and queue checks will be skipped", scenario, err)
	}

	ctx := context.Background()
	pool, err := connect(ctx, envOr("POSTGRES_URL", "postgres://route:routepassword@postgres:5432/route?sslmode=disable"))
	if err != nil {
		return err
	}
	defer pool.Close()
	httpClient := &http.Client{}

	simPath := filepath.Join(resultsDir, scenario+"-sim.json")
	os.Remove(simPath) // a stale file from an earlier run would end this one at once

	scrapeNow := func() (queue, shed, conflicts float64) {
		fams, err := scrape(ctx, httpClient, metricsURL)
		if err != nil {
			log.Printf("scrape: %v", err)
			return 0, 0, 0
		}
		return total(fams, "route_queue_depth"), total(fams, "route_pings_shed_total"), total(fams, "route_claim_conflicts_total")
	}

	log.Printf("watching scenario %q (label %q), sampling every %v", scenario, label, every)
	var samples []Sample
	for {
		samples = append(samples, takeSample(ctx, pool, scrapeNow))
		if _, err := os.Stat(simPath); err == nil {
			break
		}
		time.Sleep(every)
	}
	samples = append(samples, takeSample(ctx, pool, scrapeNow))

	raw, err := os.ReadFile(simPath)
	if err != nil {
		return err
	}
	res := &Result{Scenario: scenario, Label: label, BatchWindowMs: windowMs, Hardware: os.Getenv("BENCH_HARDWARE")}
	if err := json.Unmarshal(raw, &res.Sim); err != nil {
		return fmt.Errorf("%s: %w", simPath, err)
	}
	res.StartedAt, res.FinishedAt = res.Sim.StartedAt, res.Sim.FinishedAt
	res.LoadDurationS = prof.loadDurationS()
	// Time zero is when the load began, not when the simulator started: the
	// fleet connects first, and the phases (and their targets) are measured
	// from the first order.
	timeZero := res.Sim.LoadStartedAt
	if timeZero.IsZero() {
		timeZero = res.StartedAt
	}
	for i := range samples {
		samples[i].T = samples[i].At.Sub(timeZero).Seconds()
		if samples[i].QueueDepth > res.QueueDepthMax {
			res.QueueDepthMax = samples[i].QueueDepth
		}
		if samples[i].DoubleBooked > res.DoubleBooked {
			res.DoubleBooked = samples[i].DoubleBooked
		}
	}
	res.Samples = samples

	if err := fillFromDB(ctx, pool, res); err != nil {
		return err
	}
	if fams, err := scrape(ctx, httpClient, metricsURL); err == nil {
		res.Counters.PingsShed = total(fams, "route_pings_shed_total")
		res.Counters.PingsRejected = total(fams, "route_pings_rejected_total")
		res.Counters.ClaimConflicts = total(fams, "route_claim_conflicts_total")
		res.Counters.ClaimsRestored = total(fams, "route_claims_reconciled_total")
		if lag := histQuantile(histogramOf(fams, "route_ping_index_lag_seconds"), 0.99); lag == lag { // not NaN
			res.Counters.PingIndexLagP99 = lag
		}
	} else {
		log.Printf("final scrape failed: %v", err)
	}
	if res.LoadDurationS > 0 {
		res.QueueThirds, res.QueueStable = queueStability(samples, res.LoadDurationS)
	}
	if end, ok := prof.phaseEndS("spike"); ok {
		if d, drained := secondsToDrain(samples, end, 5); drained {
			res.DrainAfterSpik = &d
		}
	}
	res.Targets = targetsFor(scenario, res)

	out, _ := json.MarshalIndent(res, "", "  ")
	if err := os.WriteFile(filepath.Join(resultsDir, label+".json"), out, 0o644); err != nil {
		return err
	}
	svg := lineChart(fmt.Sprintf("%s: orders waiting during the run", label), "seconds since the first order", "orders", []series{
		{"queue depth (in the matcher)", pointsOf(samples, func(s Sample) float64 { return s.QueueDepth })},
		{"orders awaiting a match (queued or batched)", pointsOf(samples, func(s Sample) float64 { return float64(s.AwaitingMatch) })},
	})
	if err := os.WriteFile(filepath.Join(resultsDir, label+".svg"), []byte(svg), 0o644); err != nil {
		return err
	}
	log.Printf("%s: %d orders, match rate %.2f%%, match latency p50 %.2fs p99 %.2fs", label, res.Orders.Created, res.MatchRate*100, res.MatchLatencyS.P50, res.MatchLatencyS.P99)
	for _, t := range res.Targets {
		verdict := "reported"
		if t.Pass != nil {
			verdict = map[bool]string{true: "PASS", false: "MISS"}[*t.Pass]
		}
		log.Printf("  [%s] %s: target %s, measured %s", verdict, t.Name, t.Target, t.Measured)
	}
	return nil
}

func pointsOf(samples []Sample, y func(Sample) float64) [][2]float64 {
	var pts [][2]float64
	for _, s := range samples {
		if s.T >= 0 {
			pts = append(pts, [2]float64{s.T, y(s)})
		}
	}
	return pts
}

// connect retries for a while: the database may still be starting.
func connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	var lastErr error
	for i := 0; i < 60; i++ {
		pool, err := pgxpool.New(ctx, url)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return nil, fmt.Errorf("postgres not reachable: %w", lastErr)
}
