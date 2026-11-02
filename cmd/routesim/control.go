package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"route/internal/auth"
)

// The controller lets the dashboard start, stop and re-rate scenarios
// (ROUTE_SPEC.md 6.10 and 8.2). routed proxies /sim/* here behind its own
// bearer-token guard. The controller checks the token again, so it is safe if
// someone reaches its port directly.

// profileName limits what can be asked for to file names in the scenario
// directory: no path separators, no dots.
var profileName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// maxRate bounds the live rate control. It is above the heaviest committed
// scenario (500 orders/s).
const maxRate = 2000.0

type activeRun struct {
	profile *Profile
	env     *simEnv
	started time.Time
	cancel  context.CancelFunc
	done    chan struct{}
}

type controller struct {
	dir    string
	auth   *auth.Auth
	newEnv func(*Profile) *simEnv
	run    func(ctx context.Context, p *Profile, env *simEnv)

	// mu serialises start and stop, so two requests cannot both start a fleet.
	mu  sync.Mutex
	cur *activeRun
}

func newController(dir string, a *auth.Auth, newEnv func(*Profile) *simEnv, run func(context.Context, *Profile, *simEnv)) *controller {
	return &controller{dir: dir, auth: a, newEnv: newEnv, run: run}
}

func (c *controller) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", c.handleStatus)
	mux.HandleFunc("GET /profiles", c.handleProfiles)
	mux.Handle("POST /profiles/{name}/start", c.requireToken(c.handleStart))
	mux.Handle("POST /stop", c.requireToken(c.handleStop))
	mux.Handle("POST /rate", c.requireToken(c.handleRate))
	return mux
}

func (c *controller) requireToken(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := c.auth.Bearer(r); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "bearer token required"})
			return
		}
		next(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

type simStatus struct {
	Running    bool     `json:"running"`
	Profile    string   `json:"profile,omitempty"`
	Couriers   int      `json:"couriers,omitempty"`
	OrdersPerS *float64 `json:"orders_per_s,omitempty"` // in effect now
	Overridden bool     `json:"rate_overridden"`
	MaxRate    float64  `json:"max_rate"`
}

// status reports the current run. A scenario that ended by itself is not
// running, even though its record is kept until the next start.
func (c *controller) status() simStatus {
	st := simStatus{MaxRate: maxRate}
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur == nil {
		return st
	}
	select {
	case <-cur.done:
		return st
	default:
	}
	st.Running = true
	st.Profile, st.Couriers = cur.profile.Name, cur.profile.Couriers
	st.Overridden = cur.env.rateOverride.Load() != nil
	elapsed := time.Duration(0) // warming up: the first phase's rate
	if ns := cur.env.loadStarted.Load(); ns != 0 {
		elapsed = time.Since(time.Unix(0, ns))
	}
	if phase, ok := cur.profile.PhaseAt(elapsed); ok {
		r := cur.env.ordersPerS(phase)
		st.OrdersPerS = &r
	}
	return st
}

func (c *controller) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, c.status())
}

func (c *controller) handleProfiles(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "cannot read the scenario directory"})
		return
	}
	names := []string{}
	for _, e := range entries {
		name := strings.TrimSuffix(e.Name(), ".json")
		if name != e.Name() && profileName.MatchString(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	writeJSON(w, http.StatusOK, map[string][]string{"profiles": names})
}

func (c *controller) handleStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !profileName.MatchString(name) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid profile name"})
		return
	}
	path := filepath.Join(c.dir, name+".json")
	if _, err := os.Stat(path); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no such profile"})
		return
	}
	p, err := LoadProfile(path)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "profile is not valid"})
		return
	}

	c.mu.Lock()
	c.stopLocked()
	ctx, cancel := context.WithCancel(context.Background())
	run := &activeRun{profile: p, env: c.newEnv(p), started: time.Now(), cancel: cancel, done: make(chan struct{})}
	c.cur = run
	c.mu.Unlock()

	go func() {
		defer close(run.done)
		defer cancel() // a scenario that ends by itself still disconnects its fleet
		c.run(ctx, p, run.env)
	}()
	writeJSON(w, http.StatusAccepted, c.status())
}

// stopLocked stops the current run and waits for it. The caller holds c.mu.
func (c *controller) stopLocked() {
	if c.cur == nil {
		return
	}
	c.cur.cancel()
	<-c.cur.done
	c.cur = nil
}

// stop is for shutdown.
func (c *controller) stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopLocked()
}

func (c *controller) handleStop(w http.ResponseWriter, r *http.Request) {
	c.stop()
	writeJSON(w, http.StatusOK, c.status())
}

func (c *controller) handleRate(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var req struct {
		OrdersPerS *float64 `json:"orders_per_s"`
	}
	if err := dec.Decode(&req); err != nil || req.OrdersPerS == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "body must be {\"orders_per_s\": number}"})
		return
	}
	if v := *req.OrdersPerS; v < 0 || v > maxRate {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "orders_per_s is out of range"})
		return
	}
	c.mu.Lock()
	cur := c.cur
	c.mu.Unlock()
	if cur == nil || !c.status().Running {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "no scenario is running"})
		return
	}
	cur.env.rateOverride.Store(req.OrdersPerS)
	writeJSON(w, http.StatusOK, c.status())
}
