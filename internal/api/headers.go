package api

import (
	"net/http"
	"regexp"
)

// hostPattern is what a Host header must match before it is echoed into the
// Content-Security-Policy. Anything else (spaces, semicolons, quotes) could
// add directives of an attacker's choosing.
var hostPattern = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:[0-9]{1,5})?$|^\[[0-9A-Fa-f:]+\](:[0-9]{1,5})?$`)

// SecurityHeaders adds the response headers ROUTE_SPEC.md section 15 asks for.
//
// The Content-Security-Policy names the only third parties the dashboard
// uses: MapLibre from unpkg, the Inter font from Google Fonts, and map data
// from CARTO. It also lists the page's own WebSocket origin explicitly,
// because not every browser lets 'self' cover ws: and wss:. 'unsafe-inline'
// is for styles only: MapLibre and the markers set style attributes from
// script. Inline script stays blocked.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connect := "'self' https://basemaps.cartocdn.com https://*.basemaps.cartocdn.com"
		if hostPattern.MatchString(r.Host) {
			connect += " ws://" + r.Host + " wss://" + r.Host
		}

		h := w.Header()
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' https://unpkg.com; "+
				"style-src 'self' 'unsafe-inline' https://unpkg.com https://fonts.googleapis.com; "+
				"font-src https://fonts.gstatic.com; "+
				"img-src 'self' data: blob: https://basemaps.cartocdn.com https://*.basemaps.cartocdn.com; "+
				"connect-src "+connect+"; "+
				"worker-src blob:; child-src blob:; "+
				"object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=()")
		// Browsers ignore HSTS on plain HTTP, but only send it when the
		// request really arrived over TLS (directly or via a proxy).
		if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}
