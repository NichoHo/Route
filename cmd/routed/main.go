package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.17.0"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"google.golang.org/grpc"

	routeapi "route/internal/api"
	"route/internal/api/proto"
	"route/internal/auth"
	"route/internal/claims"
	"route/internal/config"
	"route/internal/events"
	"route/internal/index"
	"route/internal/ingest"
	"route/internal/matcher"
	"route/internal/offers"
	"route/internal/surge"
)

func initTracer() *sdktrace.TracerProvider {
	exporter, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithInsecure(), otlptracegrpc.WithEndpoint("jaeger:4317"))
	if err != nil {
		log.Fatalf("failed to initialize otlp exporter: %v", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceNameKey.String("routed"),
		)),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp
}

func main() {
	// Fail before touching any dependency if the API token is missing: the
	// mutating endpoints and courier tokens are derived from it.
	apiToken := os.Getenv("ROUTE_API_TOKEN")
	if len(apiToken) < auth.MinTokenLen {
		log.Fatalf("ROUTE_API_TOKEN must be set to at least %d characters", auth.MinTokenLen)
	}
	authn := auth.New(apiToken)

	tp := initTracer()
	defer func() {
		if err := tp.Shutdown(context.Background()); err != nil {
			log.Printf("Error shutting down tracer provider: %v", err)
		}
	}()

	// 1. Connect to Postgres
	pgUrl := os.Getenv("POSTGRES_URL")
	if pgUrl == "" {
		pgUrl = "postgres://route:routepassword@localhost:5432/route?sslmode=disable"
	}
	poolCfg, err := pgxpool.ParseConfig(pgUrl)
	if err != nil {
		log.Fatalf("invalid POSTGRES_URL: %v", err)
	}
	poolCfg.MaxConns = int32(config.PostgresMaxConns)
	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		log.Fatalf("failed to connect to postgres: %v", err)
	}
	defer pool.Close()

	// 2. Connect to Redis
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}
	store := index.NewStore(redisAddr, config.StaleCourierTimeout) // Stale timeout
	defer store.Close()

	redpandaAddr := os.Getenv("REDPANDA_ADDR")
	if redpandaAddr == "" {
		redpandaAddr = "localhost:9092"
	}

	outboxProducer, err := events.NewOutbox(pool, []string{redpandaAddr})
	if err != nil {
		log.Fatalf("failed to create outbox: %v", err)
	}
	defer outboxProducer.Stop()

	surgeEngine := surge.NewEngine(pool, store, outboxProducer)
	defer surgeEngine.Stop()

	// 3. Setup claims, matcher, and offer lifecycle
	claimsManager := claims.NewManager(store.GetClient())
	streamRegistry := ingest.NewStreamRegistry()

	batcher := matcher.NewBatcher(pool, store, claimsManager, nil, outboxProducer, surgeEngine)
	offerLifecycle := offers.NewLifecycle(pool, claimsManager, streamRegistry, batcher, outboxProducer)
	batcher.SetLifecycle(offerLifecycle)
	ctx, cancel := context.WithCancel(context.Background())
	offerLifecycle.StartSweep(ctx)

	// 4. Setup Ingest Handler
	historyWriter := ingest.NewHistoryWriter(pool, 1000)
	defer historyWriter.Close()
	ingestHandler := ingest.NewHandler(store, historyWriter, streamRegistry, offerLifecycle, authn, config.MaxCourierSessions)

	// 5. Setup broadcast server
	hub := routeapi.NewHub(config.MaxWSClients)
	go routeapi.NewSnapshotter(pool, store, surgeEngine).Run(ctx, hub)

	// 6. Start gRPC server
	lis, err := net.Listen("tcp", ":9090")
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}
	grpcServer := grpc.NewServer()
	proto.RegisterCourierServer(grpcServer, ingestHandler)

	go func() {
		log.Println("gRPC server listening on :9090")
		if err := grpcServer.Serve(lis); err != nil {
			log.Fatalf("failed to serve gRPC: %v", err)
		}
	}()

	// 7. Start HTTP server
	orderAPI := routeapi.NewOrderAPI(pool, batcher, outboxProducer, surgeEngine)
	guard := routeapi.NewGuard(authn, config.OrderRatePerIP, config.OrderRatePerToken)

	mux := http.NewServeMux()
	// Mutating endpoints require the bearer token.
	mux.Handle("POST /orders", guard.Wrap(orderAPI))
	mux.Handle("POST /orders/{id}/cancel", guard.Wrap(routeapi.NewCancelHandler(offerLifecycle)))
	mux.Handle("POST /couriers/{id}/token", guard.Wrap(routeapi.NewCourierTokenHandler(authn)))
	// Reads, the dashboard and its WebSocket are public.
	mux.HandleFunc("GET /orders/{id}", routeapi.NewOrderGetHandler(pool))
	mux.HandleFunc("GET /couriers", routeapi.NewCouriersHandler(store, claimsManager))
	mux.HandleFunc("GET /surge", routeapi.NewSurgeHandler(surgeEngine))
	mux.HandleFunc("GET /batches", routeapi.NewBatchesHandler(pool))
	mux.Handle("/live", hub)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.FileServer(http.Dir("./web/dashboard")))

	httpServer := &http.Server{
		Addr:              ":8080",
		Handler:           routeapi.SecurityHeaders(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second, // does not apply to hijacked WebSockets
		IdleTimeout:       60 * time.Second,
	}
	log.Println("HTTP server listening on :8080")
	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("failed to serve HTTP: %v", err)
		}
	}()

	// /metrics lives on its own port, which compose does not publish, so it is
	// reachable only from inside the Docker network (ROUTE_SPEC.md section 15).
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: ":9100", Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}
	log.Println("metrics server listening on :9100")
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("failed to serve metrics: %v", err)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	cancel()       // Stop the sweep
	batcher.Stop() // Stop the batcher

	// Stop gRPC Server with timeout
	stopped := make(chan struct{})
	go func() {
		grpcServer.GracefulStop()
		close(stopped)
	}()

	t := time.NewTimer(5 * time.Second)
	select {
	case <-stopped:
		t.Stop()
	case <-t.C:
		log.Println("gRPC GracefulStop timed out, forcing stop")
		grpcServer.Stop()
	}

	httpCtx, httpCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer httpCancel()
	metricsServer.Close()
	if err := httpServer.Shutdown(httpCtx); err != nil {
		log.Fatalf("HTTP server shutdown failed: %v", err)
	}
	log.Println("Server exiting")
}
