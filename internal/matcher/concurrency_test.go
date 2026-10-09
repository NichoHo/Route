package matcher_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"google.golang.org/grpc/metadata"

	"route/internal/api/proto"
	"route/internal/auth"
	"route/internal/claims"
	"route/internal/config"
	"route/internal/index"
	"route/internal/ingest"
	"route/internal/matcher"
	"route/internal/offers"
	"route/internal/surge"
)

type mockCourierStream struct {
	recvCh chan *proto.CourierMessage
	sendCh chan *proto.ServerMessage
	ctx    context.Context
}

func newMockCourierStream(ctx context.Context) *mockCourierStream {
	return &mockCourierStream{
		recvCh: make(chan *proto.CourierMessage, 100),
		sendCh: make(chan *proto.ServerMessage, 100),
		ctx:    ctx,
	}
}

func (m *mockCourierStream) Send(msg *proto.ServerMessage) error {
	select {
	case m.sendCh <- msg:
		return nil
	default:
		return fmt.Errorf("send buffer full")
	}
}

func (m *mockCourierStream) Recv() (*proto.CourierMessage, error) {
	msg, ok := <-m.recvCh
	if !ok {
		return nil, fmt.Errorf("stream closed")
	}
	return msg, nil
}

func (m *mockCourierStream) SetHeader(metadata.MD) error  { return nil }
func (m *mockCourierStream) SendHeader(metadata.MD) error { return nil }
func (m *mockCourierStream) SetTrailer(metadata.MD)       {}
func (m *mockCourierStream) Context() context.Context     { return m.ctx }
func (m *mockCourierStream) SendMsg(m_ interface{}) error { return nil }
func (m *mockCourierStream) RecvMsg(m_ interface{}) error { return nil }

type mockHistory struct{}

func (m *mockHistory) Write(ping *proto.Ping) {}

// TestFullConcurrency verifies that the system can handle concurrent match attempts
// under load. It spawns 100 couriers connecting to streams, inserts 50 orders,
// and ensures all 50 orders get matched without race conditions or deadlocks.
type mockPublisher struct{}

func (m *mockPublisher) Publish(ctx context.Context, topic string, payload []byte) error { return nil }

type mockOutbox struct{}

func (m *mockOutbox) Insert(ctx context.Context, tx pgx.Tx, topic string, payload []byte) error {
	return nil
}
func (m *mockOutbox) Publish(ctx context.Context, topic string, payload []byte) error { return nil }

func TestFullConcurrency(t *testing.T) {
	pgUrl := os.Getenv("POSTGRES_URL")
	if pgUrl == "" {
		pgUrl = "postgres://route:routepassword@localhost:5432/route?sslmode=disable"
	}
	pool, err := pgxpool.New(context.Background(), pgUrl)
	if err != nil {
		t.Fatalf("db connect failed: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(context.Background()); err != nil {
		t.Skip("Postgres not running, skipping test")
	}

	redisUrl := os.Getenv("REDIS_ADDR")
	if redisUrl == "" {
		redisUrl = "localhost:6379"
	}
	client := redis.NewClient(&redis.Options{Addr: redisUrl})
	defer client.Close()
	if err := client.Ping(context.Background()).Err(); err != nil {
		t.Skip("Redis not running, skipping test")
	}

	// Clean up db
	pool.Exec(context.Background(), "TRUNCATE orders, assignments, offers CASCADE")
	client.FlushDB(context.Background())

	store := index.NewStore(redisUrl, 15*time.Second)

	// Create components
	claimsMgr := claims.NewManager(client)
	registry := ingest.NewStreamRegistry()
	surgeEngine := surge.NewEngine(pool, store, &mockPublisher{})
	batcher := matcher.NewBatcher(pool, store, claimsMgr, nil, nil, surgeEngine)
	lifecycle := offers.NewLifecycle(pool, claimsMgr, registry, batcher, &mockOutbox{})
	batcher.SetLifecycle(lifecycle)
	authn := auth.New("matcher-test-api-token-01")
	handler := ingest.NewHandler(store, &mockHistory{}, registry, lifecycle, authn, 1000)

	// Spawn 100 couriers
	courierCount := 100
	var wg sync.WaitGroup
	var matches int32

	for i := 0; i < courierCount; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			courierID := fmt.Sprintf("testc-%d", id)

			stream := newMockCourierStream(metadata.NewIncomingContext(context.Background(),
				auth.CourierMetadata(courierID, authn.CourierToken(courierID))))
			go handler.Session(stream)

			// Initial ping
			stream.recvCh <- &proto.CourierMessage{
				Payload: &proto.CourierMessage_Ping{
					Ping: &proto.Ping{CourierId: courierID, Lat: config.JakartaCenterLat, Lng: config.JakartaCenterLng, Seq: 1, ClientTs: time.Now().UnixMilli()},
				},
			}

			// Courier worker loop
			for msg := range stream.sendCh {
				if offer := msg.GetOffer(); offer != nil {
					stream.recvCh <- &proto.CourierMessage{
						Payload: &proto.CourierMessage_OfferResponse{
							OfferResponse: &proto.OfferResponse{OfferId: offer.OfferId, Accept: true},
						},
					}
				} else if outcome := msg.GetOfferOutcome(); outcome != nil {
					if outcome.Success {
						atomic.AddInt32(&matches, 1)
					}
				}
			}
		}(i)
	}

	time.Sleep(1 * time.Second) // Wait for pings to process

	orderCount := 50
	var orderIDs []string
	for i := 0; i < orderCount; i++ {
		orderID := uuid.New().String()
		orderIDs = append(orderIDs, orderID)

		_, err := pool.Exec(context.Background(), `
			INSERT INTO orders (id, idempotency_key, pickup, dropoff, pickup_h3, status, created_at, updated_at, attempts, excluded_couriers)
			VALUES ($1, $2, ST_SetSRID(ST_MakePoint($3, $4), 4326), ST_SetSRID(ST_MakePoint($3, $4), 4326), 'h3', 'queued', NOW(), NOW(), 0, '{}')
		`, orderID, uuid.New().String(), config.JakartaCenterLng, config.JakartaCenterLat)
		if err != nil {
			t.Fatalf("failed to insert order: %v", err)
		}
	}

	// Trigger match batches
	for _, id := range orderIDs {
		batcher.Trigger(id)
	}

	// Poll for matches instead of a flat sleep: usually finishes well under
	// the batch window, and still gives slow CI runners the full budget.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&matches) < int32(orderCount) {
		time.Sleep(100 * time.Millisecond)
	}

	if got := atomic.LoadInt32(&matches); got != int32(orderCount) {
		t.Errorf("Expected %d matches, got %d", orderCount, got)
	}

	// The invariant this test exists to prove: no courier ever holds more
	// than one active (uncompleted) assignment at once.
	rows, err := pool.Query(context.Background(), `
		SELECT courier_id, count(*) FROM assignments
		WHERE completed_at IS NULL
		GROUP BY courier_id HAVING count(*) > 1
	`)
	if err != nil {
		t.Fatalf("failed to check for double bookings: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var courierID string
		var count int
		if err := rows.Scan(&courierID, &count); err == nil {
			t.Errorf("courier %s holds %d active assignments", courierID, count)
		}
	}
}
