package api

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"route/internal/auth"
)

// maxBuckets bounds the limiter's memory; idle buckets are evicted when it is
// reached. ponytail: if more than this many keys are active within a minute
// the map grows past it; add a hard cap or an LRU if that ever matters.
const maxBuckets = 10000

// Limiter is a token-bucket rate limiter keyed by string.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // tokens per second
	burst   float64
	buckets map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func NewLimiter(rate, burst float64) *Limiter {
	return &Limiter{rate: rate, burst: burst, buckets: make(map[string]*bucket), now: time.Now}
}

// Allow takes one token from key's bucket, reporting whether one was left.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			for k, old := range l.buckets {
				if now.Sub(old.last) > time.Minute {
					delete(l.buckets, k)
				}
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Guard protects the mutating endpoints: per-IP rate limit, then bearer
// token, then per-token rate limit. The IP limit runs first so floods of bad
// or missing tokens are cheap to reject and are also rate limited.
type Guard struct {
	auth     *auth.Auth
	perIP    *Limiter
	perToken *Limiter
}

// NewGuard takes sustained rates in requests per second; burst is twice the
// rate.
func NewGuard(a *auth.Auth, ipRate, tokenRate float64) *Guard {
	return &Guard{
		auth:     a,
		perIP:    NewLimiter(ipRate, 2*ipRate),
		perToken: NewLimiter(tokenRate, 2*tokenRate),
	}
}

func (g *Guard) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.perIP.Allow(clientIP(r)) {
			tooManyRequests(w)
			return
		}
		token, ok := g.auth.Bearer(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if !g.perToken.Allow(token) {
			tooManyRequests(w)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func tooManyRequests(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "1")
	http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
}

// clientIP is the connection's peer address. Behind a reverse proxy this is
// the proxy's address, so per-IP limiting degrades to one shared bucket;
// trust X-Forwarded-For only from a configured proxy if that deployment
// shape is ever used.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// NewCourierTokenHandler serves POST /couriers/{id}/token, issuing the gRPC
// session token for one courier. Wrap it in a Guard.
func NewCourierTokenHandler(a *auth.Auth) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !auth.ValidCourierID(id) {
			http.Error(w, "invalid courier id", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"token": a.CourierToken(id)})
	}
}
