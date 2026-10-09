package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"route/internal/auth"
)

const guardToken = "guard-test-token-0123456789"

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func do(h http.Handler, remote, authz string) int {
	r := httptest.NewRequest("POST", "/orders", nil)
	r.RemoteAddr = remote
	if authz != "" {
		r.Header.Set("Authorization", authz)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
}

func TestGuardRequiresBearer(t *testing.T) {
	h := NewGuard(auth.New(guardToken), 1000, 1000).Wrap(okHandler())

	if got := do(h, "1.2.3.4:1", ""); got != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401", got)
	}
	if got := do(h, "1.2.3.4:1", "Bearer wrong"); got != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d, want 401", got)
	}
	if got := do(h, "1.2.3.4:1", "Bearer "+guardToken); got != http.StatusNoContent {
		t.Fatalf("valid token: got %d, want 204", got)
	}
}

func TestGuardRateLimitsPerIPIndependently(t *testing.T) {
	// 1 req/s, burst 2: a third immediate request from one IP is refused,
	// while a different IP is unaffected.
	h := NewGuard(auth.New(guardToken), 1, 1000).Wrap(okHandler())
	valid := "Bearer " + guardToken

	for i := 0; i < 2; i++ {
		if got := do(h, "1.2.3.4:1", valid); got != http.StatusNoContent {
			t.Fatalf("request %d: got %d, want 204", i, got)
		}
	}
	if got := do(h, "1.2.3.4:1", valid); got != http.StatusTooManyRequests {
		t.Fatalf("over limit: got %d, want 429", got)
	}
	if got := do(h, "5.6.7.8:1", valid); got != http.StatusNoContent {
		t.Fatalf("other IP: got %d, want 204", got)
	}
}

// Bad-token floods count against the IP bucket too.
func TestGuardRateLimitsFailedAuth(t *testing.T) {
	h := NewGuard(auth.New(guardToken), 1, 1000).Wrap(okHandler())
	do(h, "1.2.3.4:1", "Bearer a")
	do(h, "1.2.3.4:1", "Bearer b")
	if got := do(h, "1.2.3.4:1", "Bearer c"); got != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", got)
	}
}

func TestGuardRateLimitsPerToken(t *testing.T) {
	// Different IPs share the one token's bucket (burst 2).
	h := NewGuard(auth.New(guardToken), 1000, 1).Wrap(okHandler())
	valid := "Bearer " + guardToken

	do(h, "1.1.1.1:1", valid)
	do(h, "2.2.2.2:1", valid)
	if got := do(h, "3.3.3.3:1", valid); got != http.StatusTooManyRequests {
		t.Fatalf("got %d, want 429", got)
	}
}

func TestLimiterRefills(t *testing.T) {
	l := NewLimiter(10, 1)
	now := time.Now()
	l.now = func() time.Time { return now }

	if !l.Allow("k") || l.Allow("k") {
		t.Fatal("burst of 1 should allow exactly one immediate request")
	}
	now = now.Add(150 * time.Millisecond) // 1.5 tokens at 10/s
	if !l.Allow("k") {
		t.Fatal("bucket should have refilled")
	}
}

func TestLimiterEvictsIdleBuckets(t *testing.T) {
	l := NewLimiter(1, 1)
	now := time.Now()
	l.now = func() time.Time { return now }
	for i := 0; i < maxBuckets; i++ {
		l.buckets[string(rune(i))+"x"] = &bucket{tokens: 1, last: now}
	}
	now = now.Add(2 * time.Minute)
	l.Allow("fresh")
	if len(l.buckets) != 1 {
		t.Fatalf("expected idle buckets evicted, have %d", len(l.buckets))
	}
}

func TestCourierTokenHandler(t *testing.T) {
	a := auth.New(guardToken)
	mux := http.NewServeMux()
	mux.Handle("POST /couriers/{id}/token", NewCourierTokenHandler(a))

	r := httptest.NewRequest("POST", "/couriers/courier-1/token", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), a.CourierToken("courier-1")) {
		t.Fatalf("got %d %s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest("POST", "/couriers/bad%20id/token", nil)
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid id: got %d, want 400", w.Code)
	}
}
