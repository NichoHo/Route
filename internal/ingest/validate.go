package ingest

import (
	"math"
	"time"

	"route/internal/api/proto"
	"route/internal/auth"
)

const (
	// Redis GEOADD rejects latitudes beyond this (the Web Mercator limit),
	// so anything outside it can never be indexed.
	maxGeoLat = 85.05112878

	// maxClientClockSkew bounds how far client_ts may be from server time.
	// It is a sanity check only: staleness and deadlines are judged on
	// server time (ROUTE_SPEC.md ADR 007), so this is deliberately far wider
	// than the +-30s skew the chaos test injects.
	maxClientClockSkew = 5 * time.Minute
)

// Rejection reasons. They double as the `reason` label on
// route_pings_rejected_total, so keep the set small and fixed.
const (
	reasonCourierID   = "courier_id"
	reasonMismatch    = "courier_mismatch"
	reasonCoordinates = "coordinates"
	reasonHeading     = "heading"
	reasonSeq         = "seq"
	reasonClientTS    = "client_ts"
)

// validatePing returns "" for an acceptable ping, otherwise the rejection
// reason. Comparisons are written so NaN fails them.
func validatePing(p *proto.Ping, now time.Time) string {
	if !auth.ValidCourierID(p.CourierId) {
		return reasonCourierID
	}
	if !(p.Lat >= -maxGeoLat && p.Lat <= maxGeoLat && p.Lng >= -180 && p.Lng <= 180) {
		return reasonCoordinates
	}
	if !(p.Heading >= 0 && p.Heading <= 360) {
		return reasonHeading
	}
	if p.Seq < 0 {
		return reasonSeq
	}
	skew := now.UnixMilli() - p.ClientTs
	if math.Abs(float64(skew)) > float64(maxClientClockSkew.Milliseconds()) {
		return reasonClientTS
	}
	return ""
}
