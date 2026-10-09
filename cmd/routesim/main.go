// routesim is the courier client and order generator. It is the demo, the
// load generator and the only courier client (ROUTE_SPEC.md section 6.8).
//
// With no SIM_PROFILE it runs the original uniform demo load. With
// SIM_PROFILE=/path/to/profile.json it runs that scenario; the committed
// scenarios live in loadtest/scenarios. A scenario that is not a loop ends by
// itself, writes a summary, and exits.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"route/internal/api/proto"
)

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// couriersPerConn spreads couriers over several gRPC connections: one HTTP/2
// connection carrying thousands of streams becomes its own bottleneck.
const couriersPerConn = 1000

func main() {
	apiToken := os.Getenv("ROUTE_API_TOKEN")
	if apiToken == "" {
		log.Fatal("ROUTE_API_TOKEN must be set")
	}

	var profile *Profile
	if path := os.Getenv("SIM_PROFILE"); path != "" {
		var err error
		if profile, err = LoadProfile(path); err != nil {
			log.Fatalf("profile: %v", err)
		}
		if n := getEnvInt("SIM_COURIERS", 0); n > 0 {
			profile.Couriers = n
		}
	} else {
		profile = defaultProfile(getEnvInt("SIM_COURIERS", 50), getEnvDuration("SIM_ORDER_INTERVAL", 2*time.Second))
	}
	seed := int64(getEnvInt("SIM_SEED", int(time.Now().UnixNano()&0x7fffffff)))
	log.Printf("profile %q: %d couriers, %d phases, loop=%v, seed=%d", profile.Name, profile.Couriers, len(profile.Phases), profile.Loop, seed)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	addr := envOr("ROUTED_ADDR", "localhost:9090")
	nConns := (profile.Couriers + couriersPerConn - 1) / couriersPerConn
	clients := make([]proto.CourierClient, nConns)
	for i := range clients {
		conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			log.Fatalf("failed to create client for routed: %v", err)
		}
		defer conn.Close()
		clients[i] = proto.NewCourierClient(conn)
	}

	env := &simEnv{
		httpAddr:   envOr("ROUTED_HTTP_ADDR", "http://localhost:8080"),
		apiToken:   apiToken,
		httpClient: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: profile.Workers + 64}},
		stats:      newStats(),
	}

	started := time.Now()
	pingEvery := time.Duration(profile.PingIntervalMs) * time.Millisecond

	// Couriers connect spread over the ramp-up, not all in one instant.
	go func() {
		gap := time.Duration(0)
		if profile.RampUpS > 0 {
			gap = secs(profile.RampUpS) / time.Duration(profile.Couriers)
		}
		for i := 0; i < profile.Couriers; i++ {
			c := newCourier(fmt.Sprintf("courier-%d", i), profile, rand.New(rand.NewSource(seed+int64(i))))
			c.client = clients[i/couriersPerConn]
			go c.run(ctx, env, pingEvery)
			if gap > 0 {
				select {
				case <-ctx.Done():
					return
				case <-time.After(gap):
				}
			}
		}
	}()

	// Orders start once the fleet has connected, plus a few seconds for the
	// first pings to land in the index. Otherwise the first orders face a fleet
	// that is still arriving, and that shows up as a long latency tail that has
	// nothing to do with matching.
	warmup := secs(profile.RampUpS) + 3*time.Second
	log.Printf("waiting %.0fs for the fleet to connect before sending orders", warmup.Seconds())
	select {
	case <-ctx.Done():
	case <-time.After(warmup):
	}
	loadStarted := time.Now()

	// Order generation finishes when the scenario does (never, for a loop).
	runOrders(ctx, profile, env, newSampler(profile, rand.New(rand.NewSource(seed))), strconv.FormatInt(started.UnixNano(), 36))

	if ctx.Err() == nil && profile.DrainS > 0 {
		log.Printf("scenario over; draining for %.0fs so in-flight orders can finish", profile.DrainS)
		select {
		case <-ctx.Done():
		case <-time.After(secs(profile.DrainS)):
		}
	}
	stop()

	summary := buildSummary(profile, env.stats, started, loadStarted, time.Now())
	out, _ := json.MarshalIndent(summary, "", "  ")
	log.Printf("summary:\n%s", out)
	if dir := os.Getenv("SIM_RESULTS_DIR"); dir != "" {
		if err := writeSummary(dir, profile.Name, out); err != nil {
			log.Fatalf("write summary: %v", err)
		}
	}
}

// Summary is what a run reports and what routebench reads to find the run's
// time window.
type Summary struct {
	Profile     string    `json:"profile"`
	Description string    `json:"description"`
	StartedAt   time.Time `json:"started_at"`
	// LoadStartedAt is when the first phase began, after the fleet connected.
	LoadStartedAt time.Time `json:"load_started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	DurationS     float64   `json:"duration_s"`
	Couriers      int       `json:"couriers"`
	Orders        struct {
		Sent     int64            `json:"sent"`
		Created  int64            `json:"created"`
		Dropped  int64            `json:"dropped_by_client"`
		Errors   int64            `json:"errors"`
		ByStatus map[string]int64 `json:"by_http_status"`
	} `json:"orders"`
	APILatencyMs struct {
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

func buildSummary(p *Profile, s *Stats, started, loadStarted, finished time.Time) Summary {
	var sum Summary
	sum.Profile, sum.Description = p.Name, p.Description
	sum.StartedAt, sum.LoadStartedAt, sum.FinishedAt = started.UTC(), loadStarted.UTC(), finished.UTC()
	sum.DurationS = finished.Sub(started).Seconds()
	sum.Couriers = p.Couriers
	sum.Orders.Sent, sum.Orders.Created = s.sent.Load(), s.created.Load()
	sum.Orders.Dropped, sum.Orders.Errors = s.dropped.Load(), s.errors.Load()
	sum.Orders.ByStatus = s.statusCounts()
	sum.APILatencyMs.P50, sum.APILatencyMs.P99 = s.latencyQuantileMs(0.5), s.latencyQuantileMs(0.99)
	sum.APILatencyMs.Max = s.maxLatencyMs.Load()
	sum.Sessions.Opened, sum.Sessions.Reconnects, sum.Sessions.TokenErrors = s.sessions.Load(), s.reconnects.Load(), s.tokenErrors.Load()
	return sum
}

// writeSummary writes atomically: routebench polls for this file, so it must
// never see a half-written one.
func writeSummary(dir, name string, data []byte) error {
	final := filepath.Join(dir, name+"-sim.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
