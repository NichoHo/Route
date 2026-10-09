package api

import (
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// writeWait bounds a single write so a stalled browser can't hold its
	// writer goroutine forever.
	writeWait = 5 * time.Second
	// pongWait is how long a client may stay silent (no pong, no message)
	// before it is dropped. pingPeriod must be shorter.
	pongWait   = 30 * time.Second
	pingPeriod = 20 * time.Second
	// maxClientMessage caps what a client may send us. The dashboard sends
	// nothing, so this only has to fit control frames.
	maxClientMessage = 512
)

// Hub fans one snapshot stream out to many WebSocket clients.
//
// Snapshots are last-write-wins state, like pings (ROUTE_SPEC.md commitment
// 4), so each client has a one-slot buffer: a slow client skips frames
// instead of blocking the broadcaster or growing a queue.
type Hub struct {
	upgrader   websocket.Upgrader
	maxClients int

	mu      sync.Mutex
	clients map[*hubClient]struct{}
}

type hubClient struct {
	conn *websocket.Conn
	send chan []byte
	done chan struct{}
	once sync.Once
}

// NewHub returns a hub that accepts at most maxClients concurrent
// connections. Cross-origin upgrades are refused (gorilla's default check
// only allows an Origin whose host matches the request's Host).
func NewHub(maxClients int) *Hub {
	return &Hub{
		upgrader:   websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024},
		maxClients: maxClients,
		clients:    make(map[*hubClient]struct{}),
	}
}

// Len reports the number of connected clients.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients)
}

func (h *Hub) full() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.clients) >= h.maxClients
}

// Broadcast hands data to every client without blocking. A client whose
// slot is still occupied has its stale frame replaced by this one.
func (h *Hub) Broadcast(data []byte) {
	h.mu.Lock()
	clients := make([]*hubClient, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()

	for _, c := range clients {
		select {
		case c.send <- data:
		default:
			select {
			case <-c.send: // drop the stale frame
			default:
			}
			select {
			case c.send <- data:
			default:
			}
		}
	}
}

// ServeHTTP upgrades the request and runs the connection until it closes.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.full() {
		http.Error(w, "too many connections", http.StatusServiceUnavailable)
		return
	}
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade has already written the HTTP error response.
		log.Printf("websocket upgrade refused: %v", err)
		return
	}

	c := &hubClient{conn: conn, send: make(chan []byte, 1), done: make(chan struct{})}

	// Authoritative cap check: the pre-check above can race with other
	// upgrades in flight.
	h.mu.Lock()
	if len(h.clients) >= h.maxClients {
		h.mu.Unlock()
		conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many connections"),
			time.Now().Add(writeWait))
		conn.Close()
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	go h.writePump(c)
	h.readPump(c) // returns when the client goes away
}

func (h *Hub) remove(c *hubClient) {
	c.once.Do(func() {
		h.mu.Lock()
		delete(h.clients, c)
		h.mu.Unlock()
		close(c.done)
		c.conn.Close()
	})
}

// readPump exists to process control frames (pong, close) and notice a
// disconnect promptly. Data frames from the client are discarded.
func (h *Hub) readPump(c *hubClient) {
	defer h.remove(c)
	c.conn.SetReadLimit(maxClientMessage)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error {
		return c.conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	for {
		if _, _, err := c.conn.ReadMessage(); err != nil {
			return
		}
	}
}

func (h *Hub) writePump(c *hubClient) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	defer h.remove(c)
	for {
		select {
		case <-c.done:
			return
		case data := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(writeWait)); err != nil {
				return
			}
		}
	}
}
