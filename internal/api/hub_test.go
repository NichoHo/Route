package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func newHubServer(t *testing.T, maxClients int) (*Hub, string) {
	t.Helper()
	hub := NewHub(maxClients)
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	return hub, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, url string, header http.Header) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	return websocket.DefaultDialer.Dial(url, header)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestHubBroadcastReachesClients(t *testing.T) {
	hub, url := newHubServer(t, 10)
	var conns []*websocket.Conn
	for i := 0; i < 3; i++ {
		c, _, err := dial(t, url, nil)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
		conns = append(conns, c)
	}
	waitFor(t, "3 clients", func() bool { return hub.Len() == 3 })

	hub.Broadcast([]byte(`{"hello":1}`))
	for i, c := range conns {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, msg, err := c.ReadMessage()
		if err != nil || string(msg) != `{"hello":1}` {
			t.Fatalf("client %d got %q, %v", i, msg, err)
		}
	}
}

func TestHubEnforcesConnectionCap(t *testing.T) {
	hub, url := newHubServer(t, 1)
	first, _, err := dial(t, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer first.Close()
	waitFor(t, "first client", func() bool { return hub.Len() == 1 })

	_, resp, err := dial(t, url, nil)
	if err == nil {
		t.Fatal("second connection should be refused")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %v", resp)
	}
}

func TestHubRemovesDisconnectedClient(t *testing.T) {
	hub, url := newHubServer(t, 10)
	c, _, err := dial(t, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitFor(t, "client registered", func() bool { return hub.Len() == 1 })
	c.Close()
	waitFor(t, "client removed", func() bool { return hub.Len() == 0 })
}

func TestHubRejectsCrossOrigin(t *testing.T) {
	_, url := newHubServer(t, 10)
	_, resp, err := dial(t, url, http.Header{"Origin": []string{"http://evil.example"}})
	if err == nil {
		t.Fatal("cross-origin upgrade should be refused")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403, got %v", resp)
	}
}

// A client that never reads must not stall Broadcast or other clients, and
// must only ever see the newest frame.
func TestHubSlowClientDoesNotBlockOthers(t *testing.T) {
	hub, url := newHubServer(t, 10)
	slow, _, err := dial(t, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer slow.Close()
	fast, _, err := dial(t, url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer fast.Close()
	waitFor(t, "2 clients", func() bool { return hub.Len() == 2 })

	payload := []byte(strings.Repeat("x", 64*1024))
	start := time.Now()
	for i := 0; i < 500; i++ {
		hub.Broadcast(payload)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Broadcast blocked for %v", elapsed)
	}

	fast.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := fast.ReadMessage(); err != nil {
		t.Fatalf("fast client starved: %v", err)
	}
}

// The HTTP server's read and write timeouts must not end a hijacked
// WebSocket.
func TestHubSurvivesServerTimeouts(t *testing.T) {
	hub := NewHub(10)
	srv := httptest.NewUnstartedServer(hub)
	srv.Config.ReadTimeout = 200 * time.Millisecond
	srv.Config.WriteTimeout = 200 * time.Millisecond
	srv.Start()
	defer srv.Close()

	c, _, err := dial(t, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	waitFor(t, "client registered", func() bool { return hub.Len() == 1 })

	time.Sleep(700 * time.Millisecond) // several multiples of both timeouts
	hub.Broadcast([]byte("still alive"))
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, msg, err := c.ReadMessage(); err != nil || string(msg) != "still alive" {
		t.Fatalf("connection did not survive server timeouts: %q, %v", msg, err)
	}
}
