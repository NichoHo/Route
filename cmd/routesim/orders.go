package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/bits"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// simEnv is what couriers and the order generator share.
type simEnv struct {
	httpAddr   string
	apiToken   string
	httpClient *http.Client
	stats      *Stats
}

// latencyBuckets: bucket i counts POST latencies in [2^(i-1), 2^i) ms, with
// bucket 0 holding anything under 1ms. 2^23 ms is over two hours.
const latencyBuckets = 24

// Stats are the counters the summary reports. Everything is safe for
// concurrent use.
type Stats struct {
	sent, created, dropped, errors    atomic.Int64
	sessions, reconnects, tokenErrors atomic.Int64
	maxLatencyMs                      atomic.Int64

	mu       sync.Mutex
	statuses map[string]int64
	latency  [latencyBuckets]atomic.Int64
}

func newStats() *Stats { return &Stats{statuses: map[string]int64{}} }

func (s *Stats) countStatus(code int) {
	s.mu.Lock()
	s.statuses[strconv.Itoa(code)]++
	s.mu.Unlock()
}

func (s *Stats) statusCounts() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.statuses))
	for k, v := range s.statuses {
		out[k] = v
	}
	return out
}

func (s *Stats) observeLatency(d time.Duration) {
	ms := d.Milliseconds()
	i := bits.Len64(uint64(ms))
	if i >= latencyBuckets {
		i = latencyBuckets - 1
	}
	s.latency[i].Add(1)
	for {
		cur := s.maxLatencyMs.Load()
		if ms <= cur || s.maxLatencyMs.CompareAndSwap(cur, ms) {
			return
		}
	}
}

// latencyQuantileMs returns the upper bound of the bucket holding quantile q.
// It is coarse (power-of-two buckets) and measured from the client, so treat
// it as the API's latency as seen by the simulator, not as match latency.
func (s *Stats) latencyQuantileMs(q float64) float64 {
	var counts [latencyBuckets]int64
	var total int64
	for i := range counts {
		counts[i] = s.latency[i].Load()
		total += counts[i]
	}
	if total == 0 {
		return 0
	}
	target := int64(float64(total)*q + 0.999999)
	var seen int64
	for i, n := range counts {
		seen += n
		if seen >= target {
			if i == 0 {
				return 1
			}
			return float64(int64(1) << i)
		}
	}
	return float64(s.maxLatencyMs.Load())
}

type orderJob struct {
	pickupLat, pickupLng   float64
	dropoffLat, dropoffLng float64
	key                    string
}

// runOrders generates orders as a Poisson process following the profile's
// phases, until the scenario ends or ctx is cancelled. Arrivals per tick are
// drawn from the actual elapsed time, so a stalled tick catches up instead of
// under-delivering. Orders go to a worker pool; if the pool falls behind the
// order is counted as dropped rather than queued without bound.
func runOrders(ctx context.Context, p *Profile, env *simEnv, smp *sampler, runID string) {
	jobs := make(chan orderJob, 100000)
	var wg sync.WaitGroup
	for i := 0; i < p.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				postOrder(ctx, env, j)
			}
		}()
	}
	defer func() {
		close(jobs)
		wg.Wait()
	}()

	const tick = 10 * time.Millisecond
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	start := time.Now()
	last := start
	n := 0
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			phase, running := p.PhaseAt(now.Sub(start))
			if !running {
				return
			}
			arrivals := poisson(smp.rng, phase.OrdersPerS*now.Sub(last).Seconds())
			last = now
			for i := 0; i < arrivals; i++ {
				pLat, pLng := smp.pickup(phase)
				dLat, dLng := smp.dropoff(pLat, pLng)
				n++
				job := orderJob{pLat, pLng, dLat, dLng, fmt.Sprintf("sim-%s-%d", runID, n)}
				select {
				case jobs <- job:
					env.stats.sent.Add(1)
				default:
					env.stats.dropped.Add(1)
				}
			}
		}
	}
}

func postOrder(ctx context.Context, env *simEnv, j orderJob) {
	payload, _ := json.Marshal(map[string]interface{}{
		"pickup_lat": j.pickupLat, "pickup_lng": j.pickupLng,
		"dropoff_lat": j.dropoffLat, "dropoff_lng": j.dropoffLng,
		"priority": 1,
	})
	req, err := http.NewRequestWithContext(ctx, "POST", env.httpAddr+"/orders", bytes.NewReader(payload))
	if err != nil {
		env.stats.errors.Add(1)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", j.key)
	req.Header.Set("Authorization", "Bearer "+env.apiToken)

	began := time.Now()
	resp, err := env.httpClient.Do(req)
	env.stats.observeLatency(time.Since(began))
	if err != nil {
		env.stats.errors.Add(1)
		if ctx.Err() == nil {
			log.Printf("order create failed: %v", err)
		}
		return
	}
	defer resp.Body.Close()
	env.stats.countStatus(resp.StatusCode)
	if resp.StatusCode != http.StatusCreated {
		return
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil || created.ID == "" {
		env.stats.errors.Add(1)
		return
	}
	env.stats.created.Add(1)
	orderLocationsMu.Lock()
	orderLocations[created.ID] = orderLocation{j.pickupLat, j.pickupLng, j.dropoffLat, j.dropoffLng}
	orderLocationsMu.Unlock()
}
