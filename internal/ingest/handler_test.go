package ingest

import (
	"context"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"route/internal/api/proto"
	"route/internal/auth"
)

type mockStore struct {
	mu      sync.Mutex
	updates int
}

func (m *mockStore) UpdatePosition(ctx context.Context, courierID string, lat, lng, heading float64) error {
	m.mu.Lock()
	m.updates++
	m.mu.Unlock()
	return nil
}

type mockWriter struct {
	mu     sync.Mutex
	writes int
}

func (m *mockWriter) Write(ping *proto.Ping) {
	m.mu.Lock()
	m.writes++
	m.mu.Unlock()
}

type mockStream struct {
	grpc.ServerStream
	pings []*proto.Ping
	ctx   context.Context
}

func (m *mockStream) Context() context.Context {
	return m.ctx
}

func (m *mockStream) Recv() (*proto.CourierMessage, error) {
	if len(m.pings) == 0 {
		// Wait forever
		<-m.ctx.Done()
		return nil, m.ctx.Err()
	}
	ping := m.pings[0]
	m.pings = m.pings[1:]
	time.Sleep(10 * time.Millisecond)
	return &proto.CourierMessage{
		Payload: &proto.CourierMessage_Ping{Ping: ping},
	}, nil
}

func (m *mockStream) Send(msg *proto.ServerMessage) error {
	return nil
}

func TestHandler_DropOutOfOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &mockStore{}
	writer := &mockWriter{}
	registry := NewStreamRegistry()
	a := auth.New(testAPIToken)
	handler := NewHandler(store, writer, registry, nil, a, 10)

	stream := &mockStream{
		ctx: authedCtx(ctx, a, "c1"),
		pings: []*proto.Ping{
			{CourierId: "c1", Seq: 1, ClientTs: nowMs()},
			{CourierId: "c1", Seq: 0, ClientTs: nowMs()}, // out of order
			{CourierId: "c1", Seq: 2, ClientTs: nowMs()},
		},
	}

	go handler.Session(stream)
	time.Sleep(100 * time.Millisecond) // let it process
	cancel()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.updates != 2 {
		t.Errorf("expected 2 updates, got %d", store.updates)
	}
}

func nowMs() int64 { return time.Now().UnixMilli() }

func TestValidatePing(t *testing.T) {
	now := time.Now()
	ok := func() *proto.Ping {
		return &proto.Ping{CourierId: "courier-1", Lat: -6.2, Lng: 106.8, Heading: 90, Seq: 1, ClientTs: now.UnixMilli()}
	}
	nan := math.NaN()
	cases := []struct {
		name   string
		mutate func(p *proto.Ping)
		want   string
	}{
		{"valid", func(p *proto.Ping) {}, ""},
		{"valid heading 360", func(p *proto.Ping) { p.Heading = 360 }, ""},
		{"skew within window", func(p *proto.Ping) { p.ClientTs = now.Add(-30 * time.Second).UnixMilli() }, ""},
		{"empty id", func(p *proto.Ping) { p.CourierId = "" }, reasonCourierID},
		{"id with colon", func(p *proto.Ping) { p.CourierId = "a:b" }, reasonCourierID},
		{"id with newline", func(p *proto.Ping) { p.CourierId = "a\nb" }, reasonCourierID},
		{"id too long", func(p *proto.Ping) { p.CourierId = strings.Repeat("a", auth.MaxCourierIDLen+1) }, reasonCourierID},
		{"lat too high", func(p *proto.Ping) { p.Lat = 90 }, reasonCoordinates},
		{"lat beyond redis geo limit", func(p *proto.Ping) { p.Lat = 85.06 }, reasonCoordinates},
		{"lng out of range", func(p *proto.Ping) { p.Lng = 181 }, reasonCoordinates},
		{"nan lat", func(p *proto.Ping) { p.Lat = nan }, reasonCoordinates},
		{"inf lng", func(p *proto.Ping) { p.Lng = math.Inf(1) }, reasonCoordinates},
		{"negative heading", func(p *proto.Ping) { p.Heading = -1 }, reasonHeading},
		{"nan heading", func(p *proto.Ping) { p.Heading = nan }, reasonHeading},
		{"negative seq", func(p *proto.Ping) { p.Seq = -1 }, reasonSeq},
		{"missing client_ts", func(p *proto.Ping) { p.ClientTs = 0 }, reasonClientTS},
		{"client_ts far in past", func(p *proto.Ping) { p.ClientTs = now.Add(-time.Hour).UnixMilli() }, reasonClientTS},
		{"client_ts far in future", func(p *proto.Ping) { p.ClientTs = now.Add(time.Hour).UnixMilli() }, reasonClientTS},
	}
	for _, c := range cases {
		p := ok()
		c.mutate(p)
		if got := validatePing(p, now); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Invalid pings must be dropped without touching the store; a later valid
// ping on the same stream still works.
func TestHandler_RejectsInvalidPings(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &mockStore{}
	writer := &mockWriter{}
	registry := NewStreamRegistry()
	a := auth.New(testAPIToken)
	handler := NewHandler(store, writer, registry, nil, a, 10)

	stream := &mockStream{
		ctx: authedCtx(ctx, a, "c1"),
		pings: []*proto.Ping{
			{CourierId: "c2", Lat: -6.2, Lng: 106.8, Seq: 1, ClientTs: nowMs()}, // not this session's courier
			{CourierId: "c1", Lat: 95, Lng: 106.8, Seq: 2, ClientTs: nowMs()},
			{CourierId: "c1", Lat: -6.2, Lng: 106.8, Seq: 3, ClientTs: nowMs() - int64(time.Hour/time.Millisecond)},
			{CourierId: "c1", Lat: -6.2, Lng: 106.8, Seq: 4, ClientTs: nowMs()},
		},
	}

	go handler.Session(stream)
	time.Sleep(200 * time.Millisecond)
	cancel()

	store.mu.Lock()
	defer store.mu.Unlock()
	if store.updates != 1 {
		t.Errorf("expected 1 accepted ping, got %d", store.updates)
	}
}

const testAPIToken = "ingest-test-api-token-0123"

func authedCtx(ctx context.Context, a *auth.Auth, id string) context.Context {
	return metadata.NewIncomingContext(ctx, auth.CourierMetadata(id, a.CourierToken(id)))
}

func TestHandler_RejectsBadCredentials(t *testing.T) {
	a := auth.New(testAPIToken)
	other := auth.New("some-other-api-token-0123")
	bg := context.Background()
	cases := []struct {
		name string
		ctx  context.Context
	}{
		{"no metadata", bg},
		{"wrong token", metadata.NewIncomingContext(bg, auth.CourierMetadata("c1", "nope"))},
		{"token for another courier", metadata.NewIncomingContext(bg, auth.CourierMetadata("c1", a.CourierToken("c2")))},
		{"token from another server", authedCtx(bg, other, "c1")},
		{"invalid id", authedCtx(bg, a, "bad id!")},
	}
	for _, c := range cases {
		registry := NewStreamRegistry()
		handler := NewHandler(&mockStore{}, &mockWriter{}, registry, nil, a, 10)
		err := handler.Session(&mockStream{ctx: c.ctx})
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("%s: got %v, want Unauthenticated", c.name, err)
		}
		if registry.HasStream("c1") {
			t.Errorf("%s: rejected session must not register a stream", c.name)
		}
	}
}

func TestHandler_SessionCap(t *testing.T) {
	a := auth.New(testAPIToken)
	registry := NewStreamRegistry()
	handler := NewHandler(&mockStore{}, &mockWriter{}, registry, nil, a, 1)

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- handler.Session(&mockStream{ctx: authedCtx(ctx1, a, "c1")}) }()
	deadline := time.Now().Add(2 * time.Second)
	for !registry.HasStream("c1") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	err := handler.Session(&mockStream{ctx: authedCtx(context.Background(), a, "c2")})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second session: got %v, want ResourceExhausted", err)
	}

	// Closing the first session frees its slot.
	cancel1()
	<-done1
	ctx3, cancel3 := context.WithCancel(context.Background())
	defer cancel3()
	done3 := make(chan error, 1)
	go func() { done3 <- handler.Session(&mockStream{ctx: authedCtx(ctx3, a, "c2")}) }()
	deadline = time.Now().Add(2 * time.Second)
	for !registry.HasStream("c2") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if !registry.HasStream("c2") {
		t.Fatal("slot was not released after the first session ended")
	}
	cancel3()
	<-done3
}
