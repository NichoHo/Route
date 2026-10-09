package matcher

import (
	"math"
	"math/rand"
	"testing"
	"time"

	"route/internal/config"
	"route/internal/index"
)

var (
	t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	// courier sits 1km-ish north of the order.
	pickupLat, pickupLng = -6.2, 106.82
	courierNear          = index.CourierState{ID: "c", Lat: pickupLat + 0.009, Lng: pickupLng}

	allWeights = config.ScoringWeights{Dist: 1, Wait: 10, WaitCap: 60 * time.Second, Accept: 500, Load: 300, Priority: 100}
)

func orderAged(age time.Duration, priority int) Order {
	return Order{PickupLat: pickupLat, PickupLng: pickupLng, CreatedAt: t0.Add(-age), Priority: priority}
}

// in returns inputs for an average courier in a zone with no pressure, so a
// test only moves the one input it is about.
func in(rate, pressure float64) ScoreInputs {
	return ScoreInputs{Now: t0, AcceptRate: rate, ZonePressure: pressure}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Each test below turns every weight off except one, so it checks one term.

func TestCostDistanceTerm(t *testing.T) {
	w := config.ScoringWeights{Dist: 2}
	dist := Haversine(pickupLat, pickupLng, courierNear.Lat, courierNear.Lng)
	if got := Cost(w, orderAged(0, 0), courierNear, in(1, 1)); !near(got, 2*dist) {
		t.Fatalf("got %v, want %v", got, 2*dist)
	}
	if dist < 900 || dist > 1100 {
		t.Fatalf("test setup: courier should be about 1km away, got %vm", dist)
	}
}

func TestCostWaitTerm(t *testing.T) {
	w := config.ScoringWeights{Wait: 10, WaitCap: 60 * time.Second}
	cases := []struct {
		age  time.Duration
		want float64
	}{
		{0, 600}, // just created: the most time left under the cap, so the highest cost
		{30 * time.Second, 300},
		{60 * time.Second, 0},
		{10 * time.Minute, 0},   // saturates at the cap
		{-5 * time.Second, 600}, // created "in the future" (clock skew): not below zero age
	}
	for _, c := range cases {
		if got := Cost(w, orderAged(c.age, 0), courierNear, in(1, 1)); !near(got, c.want) {
			t.Errorf("age %v: got %v, want %v", c.age, got, c.want)
		}
	}
}

func TestCostAcceptTerm(t *testing.T) {
	w := config.ScoringWeights{Accept: 500}
	cases := []struct {
		rate, want float64
	}{
		{1, 0}, {0, 500}, {0.5, 250},
		{2, 0},            // above range
		{-1, 500},         // below range
		{math.NaN(), 500}, // unknown counts as worst
	}
	for _, c := range cases {
		if got := Cost(w, orderAged(0, 0), courierNear, in(c.rate, 1)); !near(got, c.want) {
			t.Errorf("rate %v: got %v, want %v", c.rate, got, c.want)
		}
	}
}

func TestCostLoadTerm(t *testing.T) {
	w := config.ScoringWeights{Load: 300}
	cases := []struct {
		pressure, want float64
	}{{0, 300}, {1, 0}, {0.5, 150}, {9, 0}, {-1, 300}}
	for _, c := range cases {
		if got := Cost(w, orderAged(0, 0), courierNear, in(1, c.pressure)); !near(got, c.want) {
			t.Errorf("pressure %v: got %v, want %v", c.pressure, got, c.want)
		}
	}
}

func TestCostPriorityTerm(t *testing.T) {
	w := config.ScoringWeights{Priority: 100}
	cases := []struct {
		priority int
		want     float64
	}{{config.MaxPriority, 0}, {0, 900}, {5, 400}, {100, 0}, {-3, 900}}
	for _, c := range cases {
		if got := Cost(w, orderAged(0, c.priority), courierNear, in(1, 1)); !near(got, c.want) {
			t.Errorf("priority %d: got %v, want %v", c.priority, got, c.want)
		}
	}
}

// The terms add up: the full cost is the sum of each term on its own.
func TestCostIsTheSumOfItsTerms(t *testing.T) {
	o := orderAged(25*time.Second, 3)
	inputs := in(0.7, 0.4)
	full := Cost(allWeights, o, courierNear, inputs)

	var sum float64
	for _, w := range []config.ScoringWeights{
		{Dist: allWeights.Dist},
		{Wait: allWeights.Wait, WaitCap: allWeights.WaitCap},
		{Accept: allWeights.Accept},
		{Load: allWeights.Load},
		{Priority: allWeights.Priority},
	} {
		sum += Cost(w, o, courierNear, inputs)
	}
	if !near(full, sum) {
		t.Fatalf("full cost %v, sum of terms %v", full, sum)
	}
}

// Better in any one respect, equal in the rest, means strictly cheaper.
func TestCostFavoursTheBetterCandidate(t *testing.T) {
	base := Cost(allWeights, orderAged(10*time.Second, 2), courierNear, in(0.6, 0.3))
	farther := index.CourierState{Lat: courierNear.Lat + 0.005, Lng: courierNear.Lng}

	cases := []struct {
		name string
		got  float64
	}{
		{"closer courier", Cost(allWeights, orderAged(10*time.Second, 2), index.CourierState{Lat: pickupLat + 0.001, Lng: pickupLng}, in(0.6, 0.3))},
		{"older order", Cost(allWeights, orderAged(40*time.Second, 2), courierNear, in(0.6, 0.3))},
		{"more reliable courier", Cost(allWeights, orderAged(10*time.Second, 2), courierNear, in(0.9, 0.3))},
		{"hotter zone", Cost(allWeights, orderAged(10*time.Second, 2), courierNear, in(0.6, 0.8))},
		{"higher priority", Cost(allWeights, orderAged(10*time.Second, 2+3), courierNear, in(0.6, 0.3))},
	}
	for _, c := range cases {
		if !(c.got < base) {
			t.Errorf("%s: cost %v is not below the baseline %v", c.name, c.got, base)
		}
	}
	if got := Cost(allWeights, orderAged(10*time.Second, 2), farther, in(0.6, 0.3)); !(got > base) {
		t.Errorf("farther courier: cost %v is not above the baseline %v", got, base)
	}
}

// Hungarian assumes non-negative costs, and batch totals are only comparable
// if no term can go negative. Hold for any inputs, including garbage.
func TestCostIsNeverNegative(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	odd := []float64{0, 1, -1, 0.5, 2, math.NaN(), math.Inf(1), math.Inf(-1), 1e12}
	pick := func() float64 { return odd[rng.Intn(len(odd))] }
	for i := 0; i < 20000; i++ {
		w := config.ScoringWeights{
			Dist: rng.Float64() * 5, Wait: rng.Float64() * 50, Accept: rng.Float64() * 2000,
			Load: rng.Float64() * 2000, Priority: rng.Float64() * 500,
			WaitCap: time.Duration(rng.Intn(300)) * time.Second,
		}
		o := orderAged(time.Duration(rng.Int63n(int64(10*time.Minute)))-time.Minute, rng.Intn(40)-15)
		inputs := ScoreInputs{Now: t0, AcceptRate: pick(), ZonePressure: pick()}
		if rng.Intn(2) == 0 {
			inputs.AcceptRate, inputs.ZonePressure = rng.Float64(), rng.Float64()
		}
		got := Cost(w, o, courierNear, inputs)
		if math.IsNaN(got) || math.IsInf(got, 0) || got < 0 {
			t.Fatalf("cost %v for weights %+v order %+v inputs %+v", got, w, o, inputs)
		}
	}
}

func TestSmoothedAcceptRate(t *testing.T) {
	if got := SmoothedAcceptRate(0, 0); !near(got, config.AcceptRatePrior) {
		t.Errorf("no history: got %v, want the prior %v", got, config.AcceptRatePrior)
	}
	if got := SmoothedAcceptRate(100, 0); got < 0.95 || got > 1 {
		t.Errorf("always accepts: got %v, want close to 1", got)
	}
	if got := SmoothedAcceptRate(0, 100); got > 0.1 || got < 0 {
		t.Errorf("never accepts: got %v, want close to 0", got)
	}
	// One decline barely moves a long record, but a short record moves a lot.
	long := SmoothedAcceptRate(50, 1)
	short := SmoothedAcceptRate(1, 1)
	if !(long > short) {
		t.Errorf("long good record (%v) should outrank a thin one (%v)", long, short)
	}
	// More accepts is never worse; more rejections is never better.
	for n := 0; n < 20; n++ {
		if SmoothedAcceptRate(n+1, 3) < SmoothedAcceptRate(n, 3) || SmoothedAcceptRate(3, n+1) > SmoothedAcceptRate(3, n) {
			t.Fatalf("not monotone at n=%d", n)
		}
	}
}

func TestZonePressure(t *testing.T) {
	cases := []struct{ multiplier, want float64 }{
		{1, 0}, {2, 0.5}, {3, 1}, {0.5, 0}, {10, 1}, {math.NaN(), 0},
	}
	for _, c := range cases {
		if got := ZonePressure(c.multiplier); !near(got, c.want) {
			t.Errorf("multiplier %v: got %v, want %v", c.multiplier, got, c.want)
		}
	}
}
