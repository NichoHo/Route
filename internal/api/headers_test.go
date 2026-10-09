package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serveWithHeaders(host, forwardedProto string) http.Header {
	h := SecurityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = host
	if forwardedProto != "" {
		r.Header.Set("X-Forwarded-Proto", forwardedProto)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Result().Header
}

func TestSecurityHeadersPresent(t *testing.T) {
	h := serveWithHeaders("localhost:8080", "")
	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, directive := range []string{
		"default-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'",
		"script-src 'self' https://unpkg.com", "ws://localhost:8080", "wss://localhost:8080",
	} {
		if !strings.Contains(csp, directive) {
			t.Errorf("CSP is missing %q: %s", directive, csp)
		}
	}
	if strings.Contains(csp, "script-src 'self' https://unpkg.com 'unsafe") || strings.Contains(csp, "'unsafe-eval'") {
		t.Errorf("scripts must not allow unsafe-inline or unsafe-eval: %s", csp)
	}
}

func TestSecurityHeadersHSTSOnlyOverTLS(t *testing.T) {
	if got := serveWithHeaders("localhost", "").Get("Strict-Transport-Security"); got != "" {
		t.Errorf("HSTS sent over plain HTTP: %q", got)
	}
	if got := serveWithHeaders("localhost", "https").Get("Strict-Transport-Security"); got == "" {
		t.Error("HSTS missing behind an HTTPS proxy")
	}
}

// A hostile Host header must not be able to add CSP directives.
func TestSecurityHeadersHostInjection(t *testing.T) {
	for _, host := range []string{
		"evil.example; script-src *",
		"a b",
		"x'y",
		"",
		"host\r\nSet-Cookie: x=1",
	} {
		csp := serveWithHeaders(host, "").Get("Content-Security-Policy")
		if strings.Contains(csp, "evil.example") || strings.Contains(csp, "script-src *") || strings.Contains(csp, "Set-Cookie") {
			t.Errorf("host %q leaked into the CSP: %s", host, csp)
		}
	}
	for _, host := range []string{"example.com", "10.0.0.1:8080", "[::1]:8080"} {
		if csp := serveWithHeaders(host, "").Get("Content-Security-Policy"); !strings.Contains(csp, "ws://"+host) {
			t.Errorf("valid host %q was not added to connect-src: %s", host, csp)
		}
	}
}
