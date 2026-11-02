package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"route/internal/auth"
)

const testToken = "control-test-token-0123456789"

// fakeRuns stands in for runScenario: it blocks until its context ends.
type fakeRuns struct {
	mu       sync.Mutex
	finished []string
}

func (f *fakeRuns) run(ctx context.Context, p *Profile, env *simEnv) {
	env.loadStarted.Store(time.Now().UnixNano())
	<-ctx.Done()
	f.mu.Lock()
	f.finished = append(f.finished, p.Name)
	f.mu.Unlock()
}

func newTestController(t *testing.T) (*controller, *fakeRuns) {
	t.Helper()
	dir := t.TempDir()
	for _, n := range []string{"alpha", "beta"} {
		body := `{"name":"` + n + `","couriers":3,"phases":[{"name":"p","duration_s":60,"orders_per_s":5}]}`
		if err := os.WriteFile(filepath.Join(dir, n+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "broken.json"), []byte(`{"name":"broken"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	f := &fakeRuns{}
	c := newController(dir, auth.New(testToken), func(p *Profile) *simEnv { return &simEnv{stats: newStats()} }, f.run)
	t.Cleanup(c.stop)
	return c, f
}

func do(c *controller, method, path, body string, authed bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if authed {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	rec := httptest.NewRecorder()
	c.handler().ServeHTTP(rec, req)
	return rec
}

func TestControllerNeedsTheTokenToChangeAnything(t *testing.T) {
	c, _ := newTestController(t)
	for _, tc := range [][2]string{{"POST", "/profiles/alpha/start"}, {"POST", "/stop"}, {"POST", "/rate"}} {
		if got := do(c, tc[0], tc[1], `{"orders_per_s":1}`, false).Code; got != http.StatusUnauthorized {
			t.Errorf("%s %s without a token = %d, want 401", tc[0], tc[1], got)
		}
	}
	if got := do(c, "GET", "/status", "", false).Code; got != http.StatusOK {
		t.Errorf("GET /status = %d, want 200", got)
	}
}

func TestControllerListsOnlyScenarioFiles(t *testing.T) {
	c, _ := newTestController(t)
	body := do(c, "GET", "/profiles", "", false).Body.String()
	if !strings.Contains(body, `"alpha"`) || !strings.Contains(body, `"beta"`) || strings.Contains(body, "notes") {
		t.Errorf("profiles = %s", body)
	}
}

func TestControllerRejectsBadProfileNames(t *testing.T) {
	c, _ := newTestController(t)
	for name, want := range map[string]int{
		"missing":     http.StatusNotFound,
		"broken":      http.StatusUnprocessableEntity,
		"..%2Fsecret": http.StatusBadRequest,
		"Alpha":       http.StatusBadRequest,
	} {
		if got := do(c, "POST", "/profiles/"+name+"/start", "", true).Code; got != want {
			t.Errorf("start %q = %d, want %d", name, got, want)
		}
	}
}

func TestControllerRunsOneScenarioAtATime(t *testing.T) {
	c, f := newTestController(t)
	if rec := do(c, "POST", "/profiles/alpha/start", "", true); rec.Code != http.StatusAccepted {
		t.Fatalf("start alpha = %d: %s", rec.Code, rec.Body)
	}
	if body := do(c, "GET", "/status", "", false).Body.String(); !strings.Contains(body, `"running":true`) || !strings.Contains(body, `"alpha"`) {
		t.Errorf("status = %s", body)
	}
	// Starting another stops the first before the second begins.
	do(c, "POST", "/profiles/beta/start", "", true)
	f.mu.Lock()
	finished := append([]string(nil), f.finished...)
	f.mu.Unlock()
	if len(finished) != 1 || finished[0] != "alpha" {
		t.Errorf("finished = %v, want [alpha] before beta runs", finished)
	}
	if body := do(c, "POST", "/stop", "", true).Body.String(); !strings.Contains(body, `"running":false`) {
		t.Errorf("after stop: %s", body)
	}
}

func TestControllerRateChangesTheRunningScenario(t *testing.T) {
	c, _ := newTestController(t)
	if got := do(c, "POST", "/rate", `{"orders_per_s":10}`, true).Code; got != http.StatusConflict {
		t.Errorf("rate with nothing running = %d, want 409", got)
	}
	do(c, "POST", "/profiles/alpha/start", "", true)
	rec := do(c, "POST", "/rate", `{"orders_per_s":42}`, true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"orders_per_s":42`) || !strings.Contains(rec.Body.String(), `"rate_overridden":true`) {
		t.Errorf("rate = %d %s", rec.Code, rec.Body)
	}
	for _, body := range []string{`{"orders_per_s":-1}`, `{"orders_per_s":99999}`, `{}`, `{"orders_per_s":1,"x":2}`, `nonsense`} {
		if got := do(c, "POST", "/rate", body, true).Code; got != http.StatusBadRequest {
			t.Errorf("rate %s = %d, want 400", body, got)
		}
	}
}

func TestOrdersFollowTheRateOverride(t *testing.T) {
	env := &simEnv{}
	if got := env.ordersPerS(Phase{OrdersPerS: 5}); got != 5 {
		t.Errorf("without an override = %v, want the phase's 5", got)
	}
	zero := 0.0
	env.rateOverride.Store(&zero)
	if got := env.ordersPerS(Phase{OrdersPerS: 5}); got != 0 {
		t.Errorf("with an override of 0 = %v, want 0 (paused)", got)
	}
}
