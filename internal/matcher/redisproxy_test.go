package matcher_test

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// tcpProxy sits between the matcher and Redis so a test can cut the
// connection deterministically: immediately, or after the Nth client->server
// chunk has been forwarded (so the Nth command runs on Redis but its reply is
// lost, the nastiest case for claim bookkeeping).
type tcpProxy struct {
	target string
	addr   string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
	dead  bool

	armed  atomic.Bool
	budget atomic.Int64
	sent   atomic.Int64 // client->server chunks forwarded so far
	fired  atomic.Bool  // the KillAfter budget ran out (the kill landed mid-flight)
}

func newTCPProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{target: target, addr: ln.Addr().String(), ln: ln, conns: map[net.Conn]struct{}{}}
	go p.serve(ln)
	t.Cleanup(p.Kill)
	return p
}

func (p *tcpProxy) Addr() string { return p.addr }

func (p *tcpProxy) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *tcpProxy) handle(client net.Conn) {
	server, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		client.Close()
		server.Close()
		return
	}
	p.conns[client] = struct{}{}
	p.conns[server] = struct{}{}
	p.mu.Unlock()

	go func() {
		io.Copy(client, server)
		client.Close()
		server.Close()
	}()

	buf := make([]byte, 32*1024)
	for {
		n, err := client.Read(buf)
		if n > 0 {
			server.Write(buf[:n])
			p.sent.Add(1)
			if p.armed.Load() && p.budget.Add(-1) <= 0 {
				p.armed.Store(false)
				p.fired.Store(true)
				go p.Kill() // after forwarding: the command ran, the reply is lost
			}
		}
		if err != nil {
			break
		}
	}
	client.Close()
	server.Close()
}

// KillAfter cuts Redis once n more client chunks have been forwarded. n <= 0
// cuts it now.
func (p *tcpProxy) KillAfter(n int) {
	if n <= 0 {
		p.Kill()
		return
	}
	p.budget.Store(int64(n))
	p.armed.Store(true)
}

// Kill drops every live connection and refuses new ones.
func (p *tcpProxy) Kill() {
	p.armed.Store(false)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dead = true
	p.ln.Close()
	for c := range p.conns {
		c.Close()
	}
	p.conns = map[net.Conn]struct{}{}
}

// Restart accepts connections again on the same address.
func (p *tcpProxy) Restart() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.dead {
		return
	}
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		panic("proxy restart: " + err.Error())
	}
	p.ln = ln
	p.dead = false
	go p.serve(ln)
}
