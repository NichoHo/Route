package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSimProxyRewritesThePathAndKeepsTheToken(t *testing.T) {
	var gotPath, gotAuth, gotMethod string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth, gotMethod = r.URL.Path, r.Header.Get("Authorization"), r.Method
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	proxy, err := NewSimProxy(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/sim/profiles/baseline/start", nil)
	req.Header.Set("Authorization", "Bearer abc")
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted || gotPath != "/profiles/baseline/start" || gotAuth != "Bearer abc" || gotMethod != "POST" {
		t.Errorf("code=%d path=%q auth=%q method=%q", rec.Code, gotPath, gotAuth, gotMethod)
	}
}

func TestSimProxyHidesWhyTheSimulatorIsDown(t *testing.T) {
	backend := httptest.NewServer(http.NotFoundHandler())
	url := backend.URL
	backend.Close() // nothing listens there now

	proxy, _ := NewSimProxy(url)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, httptest.NewRequest("GET", "/sim/status", nil))

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("code = %d, want 502", rec.Code)
	}
	if body := rec.Body.String(); strings.Contains(body, "127.0.0.1") || strings.Contains(body, "refused") {
		t.Errorf("the response leaks the dial error: %s", body)
	}
}

func TestSimProxyRejectsBadTargets(t *testing.T) {
	for _, target := range []string{"", "routesim:8090", "ftp://x", "http://"} {
		if _, err := NewSimProxy(target); err == nil {
			t.Errorf("target %q was accepted", target)
		}
	}
}
