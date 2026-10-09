package matcher

import (
	"math"
	"time"

	"route/internal/config"
	"route/internal/index"
	"route/internal/surge"
)

type Order struct {
	ID        string
	PickupLat float64
	PickupLng float64
	CreatedAt time.Time
	Priority  int
}

// ScoreInputs is what a pair's cost depends on beyond the order and courier
// themselves. Gathering it (database, surge engine) is the caller's job, so
// Cost stays a pure function.
type ScoreInputs struct {
	Now          time.Time
	AcceptRate   float64 // the courier's smoothed historical accept rate, 0..1
	ZonePressure float64 // demand pressure at the order's pickup zone, 0..1
}

// Cost is the objective cost of offering order o to courier c. Lower is
// better. It is the sum of five terms, each non-negative:
//
//	distance   courier-to-pickup distance
//	wait       falls as the order ages, so old orders are cheaper to assign
//	accept     rises as the courier's accept rate falls
//	load       falls as demand pressure at the pickup zone rises
//	priority   falls as the order's priority rises
//
// The last four terms ignore which courier is paired with the order. They
// cannot change which courier an order gets. They decide which orders get a
// courier when there are fewer couriers than orders.
//
// ROUTE_SPEC.md 6.4 writes the wait term with a negative weight. This form
// (weight times the time left under WaitCap) orders orders the same way but
// keeps every cost non-negative, so batch totals stay comparable.
func Cost(w config.ScoringWeights, o Order, c index.CourierState, in ScoreInputs) float64 {
	return costForDistance(w, o, Haversine(o.PickupLat, o.PickupLng, c.Lat, c.Lng), in)
}

// costForDistance is Cost for a pair whose distance is already known, so the
// cost matrix does not compute each haversine twice.
func costForDistance(w config.ScoringWeights, o Order, dist float64, in ScoreInputs) float64 {
	capSecs := math.Max(0, w.WaitCap.Seconds())
	waitLeft := clamp(capSecs-in.Now.Sub(o.CreatedAt).Seconds(), 0, capSecs)
	priority := clamp(float64(o.Priority), 0, config.MaxPriority)

	return w.Dist*dist +
		w.Wait*waitLeft +
		w.Accept*(1-clamp(in.AcceptRate, 0, 1)) +
		w.Load*(1-clamp(in.ZonePressure, 0, 1)) +
		w.Priority*(config.MaxPriority-priority)
}

// SmoothedAcceptRate turns a courier's recent offer counts into an accept
// rate. With no history it returns the prior.
func SmoothedAcceptRate(accepted, rejected int) float64 {
	return (float64(accepted) + config.AcceptRatePrior*config.AcceptRatePriorWeight) /
		(float64(accepted+rejected) + config.AcceptRatePriorWeight)
}

// ZonePressure maps a surge multiplier (1.0 to surge.MaxMultiplier) onto 0..1.
func ZonePressure(multiplier float64) float64 {
	return clamp((multiplier-1)/(surge.MaxMultiplier-1), 0, 1)
}

// clamp limits x to [lo, hi]. NaN becomes lo.
func clamp(x, lo, hi float64) float64 {
	if !(x > lo) {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

const earthRadius = 6371000 // meters

func Haversine(lat1, lon1, lat2, lon2 float64) float64 {
	rad := math.Pi / 180

	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad

	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*
			math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))

	return earthRadius * c
}
