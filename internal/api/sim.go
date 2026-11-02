package api

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// NewSimProxy forwards /sim/* to the simulator's control API (ROUTE_SPEC.md
// 8.2): /sim/status goes to {target}/status, and so on. The Authorization
// header goes along, and the simulator checks the token itself. routed
// registers the proxy only when SIM_CONTROL_URL is set, so a deployment
// without a simulator has no /sim routes at all.
func NewSimProxy(target string) (http.Handler, error) {
	u, err := url.Parse(target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("SIM_CONTROL_URL must be an http(s) URL, got %q", target)
	}
	p := httputil.NewSingleHostReverseProxy(u)
	director := p.Director
	p.Director = func(r *http.Request) {
		director(r)
		r.URL.Path = strings.TrimPrefix(r.URL.Path, "/sim")
		r.URL.RawPath = ""
		r.Host = u.Host
	}
	// Starting a scenario waits for the previous fleet to disconnect.
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 10 * time.Second
	p.Transport = tr
	// The simulator's address and the dial error stay in the log, not the
	// response.
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeJSONError(w, http.StatusBadGateway, "simulator unavailable")
	}
	return p, nil
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	fmt.Fprintf(w, "{\"error\":%q}\n", msg)
}
