package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"route/internal/config"
)

// Every committed scenario must load: unknown fields, bad ranges and unknown
// hotspot names fail here, not in the middle of a ten-minute load run.
func TestCommittedScenariosAreValid(t *testing.T) {
	files, err := filepath.Glob("../../loadtest/scenarios/*.json")
	if err != nil || len(files) == 0 {
		t.Fatalf("no scenario files found: %v", err)
	}
	for _, f := range files {
		p, err := LoadProfile(f)
		if err != nil {
			t.Errorf("%v", err)
			continue
		}
		if want := strings.TrimSuffix(filepath.Base(f), ".json"); p.Name != want {
			t.Errorf("%s: name %q does not match the file name", f, p.Name)
		}
	}
}

// The section 11 scenarios are exactly the ones the spec names.
func TestSpecScenarioShapes(t *testing.T) {
	load := func(name string) *Profile {
		p, err := LoadProfile("../../loadtest/scenarios/" + name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	if p := load("baseline"); p.Couriers != 1000 || p.Phases[0].OrdersPerS != 50 || p.Phases[0].DurationS != 600 {
		t.Errorf("baseline is not 1000 couriers, 50/s, 10 min: %+v", p.Phases[0])
	}
	if p := load("throughput"); p.Couriers != 5000 || p.Phases[0].OrdersPerS != 500 || p.Phases[0].DurationS != 300 {
		t.Errorf("throughput is not 5000 couriers, 500/s, 5 min")
	}
	if p := load("ping-flood"); p.Couriers != 5000 || p.PingIntervalMs != 1000 {
		t.Errorf("ping-flood is not 5000 couriers at 1 ping/s")
	}
	if p := load("contention"); p.Couriers != 200 || p.Phases[0].OrdersPerS != 500 {
		t.Errorf("contention is not 200 couriers at 500/s")
	}
	p := load("hotspot-spike")
	var spike Phase
	for _, ph := range p.Phases {
		if ph.Name == "spike" {
			spike = ph
		}
	}
	if p.Couriers != 1000 || spike.DurationS*spike.OrdersPerS != 2000 || spike.HotspotShare != 1 || len(spike.Focus) != 1 {
		t.Errorf("hotspot-spike is not 2000 orders in 10s into one zone with 1000 couriers: %+v", spike)
	}
	e := load("e2e")
	if e.Couriers != 50 || math.Round(e.Phases[0].DurationS*e.Phases[0].OrdersPerS) != 20 || e.Phases[0].DurationS != 30 || len(e.Hotspots) != 1 {
		t.Errorf("e2e is not 50 couriers, 20 orders in 30s, one hotspot")
	}
}

func TestProfileValidation(t *testing.T) {
	good := func() *Profile {
		p := &Profile{
			Name: "x", Hotspots: []Hotspot{{Name: "h", Lat: -6.2, Lng: 106.82, SigmaM: 300, Weight: 1}},
			Phases: []Phase{{DurationS: 10, OrdersPerS: 1, HotspotShare: 0.5, Focus: []string{"h"}}},
		}
		p.applyDefaults()
		return p
	}
	if err := good().validate(); err != nil {
		t.Fatalf("good profile rejected: %v", err)
	}
	cases := map[string]func(p *Profile){
		"no name":                  func(p *Profile) { p.Name = "" },
		"no phases":                func(p *Profile) { p.Phases = nil },
		"zero duration":            func(p *Profile) { p.Phases[0].DurationS = 0 },
		"negative rate":            func(p *Profile) { p.Phases[0].OrdersPerS = -1 },
		"share above 1":            func(p *Profile) { p.Phases[0].HotspotShare = 1.5 },
		"focus on unknown hotspot": func(p *Profile) { p.Phases[0].Focus = []string{"nowhere"} },
		"hotspots used but none":   func(p *Profile) { p.Hotspots = nil; p.Phases[0].Focus = nil },
		"hotspot outside area":     func(p *Profile) { p.Hotspots[0].Lat = 10 },
		"duplicate hotspot":        func(p *Profile) { p.Hotspots = append(p.Hotspots, p.Hotspots[0]) },
		"accept mean above 1":      func(p *Profile) { p.Behavior.AcceptMean = 2 },
		"delay min above max":      func(p *Profile) { p.Behavior.ResponseDelayMs = [2]int{5000, 1000} },
		"ping interval too small":  func(p *Profile) { p.PingIntervalMs = 10 },
	}
	for name, mutate := range cases {
		p := good()
		mutate(p)
		if err := p.validate(); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
}

func TestLoadProfileRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "typo.json")
	os.WriteFile(path, []byte(`{"name":"x","coriers":5,"phases":[{"duration_s":1,"orders_per_s":1}]}`), 0o644)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("a misspelled field must be an error, not silently ignored")
	}
}

func TestPhaseAt(t *testing.T) {
	p := &Profile{Phases: []Phase{{Name: "a", DurationS: 10}, {Name: "b", DurationS: 5}}}
	at := func(s float64) (string, bool) {
		ph, ok := p.PhaseAt(secs(s))
		return ph.Name, ok
	}
	for _, c := range []struct {
		s    float64
		want string
		ok   bool
	}{{0, "a", true}, {9.9, "a", true}, {10, "b", true}, {14.9, "b", true}, {15, "", false}, {100, "", false}} {
		if got, ok := at(c.s); got != c.want || ok != c.ok {
			t.Errorf("at %vs: got (%q, %v), want (%q, %v)", c.s, got, ok, c.want, c.ok)
		}
	}
	p.Loop = true
	for _, c := range []struct {
		s    float64
		want string
	}{{15, "a"}, {26, "b"}, {30.5, "a"}} {
		if got, ok := at(c.s); got != c.want || !ok {
			t.Errorf("looping at %vs: got %q (%v), want %q", c.s, got, ok, c.want)
		}
	}
}

func meanStd(xs []float64) (mean, std float64) {
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	for _, x := range xs {
		std += (x - mean) * (x - mean)
	}
	return mean, math.Sqrt(std / float64(len(xs)))
}

func TestPoissonMatchesItsMean(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, mean := range []float64{0.005, 0.5, 5, 100} {
		xs := make([]float64, 40000)
		for i := range xs {
			xs[i] = float64(poisson(rng, mean))
		}
		m, sd := meanStd(xs)
		if math.Abs(m-mean) > 0.05*mean+0.01 {
			t.Errorf("mean %v: sample mean %v", mean, m)
		}
		// Variance of a Poisson equals its mean.
		if math.Abs(sd*sd-mean) > 0.15*mean+0.01 {
			t.Errorf("mean %v: sample variance %v", mean, sd*sd)
		}
	}
	if poisson(rng, 0) != 0 || poisson(rng, -3) != 0 {
		t.Error("a non-positive mean gives no arrivals")
	}
}

func testProfile() *Profile {
	p := &Profile{
		Name: "t",
		Hotspots: []Hotspot{
			{Name: "a", Lat: -6.1951, Lng: 106.8230, SigmaM: 400, Weight: 3},
			{Name: "b", Lat: -6.2150, Lng: 106.8300, SigmaM: 400, Weight: 1},
		},
	}
	p.applyDefaults()
	return p
}

func TestSamplerStaysInsideTheServiceArea(t *testing.T) {
	// A hotspot on the very edge forces the resample and clamp paths.
	p := testProfile()
	p.Hotspots = append(p.Hotspots, Hotspot{Name: "edge", Lat: config.JakartaMinLat, Lng: config.JakartaMinLng, SigmaM: 3000, Weight: 1})
	s := newSampler(p, rand.New(rand.NewSource(2)))
	for i := 0; i < 20000; i++ {
		ph := Phase{HotspotShare: float64(i%3) / 2}
		lat, lng := s.pickup(ph)
		if !inBBox(lat, lng) {
			t.Fatalf("pickup %v,%v is outside the service area", lat, lng)
		}
		dlat, dlng := s.dropoff(lat, lng)
		if !inBBox(dlat, dlng) {
			t.Fatalf("dropoff %v,%v is outside the service area", dlat, dlng)
		}
	}
}

func TestSamplerHotspotShareAndSpread(t *testing.T) {
	p := testProfile()
	s := newSampler(p, rand.New(rand.NewSource(3)))

	// Share 1 and a single focused hotspot: points cluster on it with about
	// the configured spread.
	var east, north []float64
	h := p.Hotspots[0]
	for i := 0; i < 20000; i++ {
		lat, lng := s.pickup(Phase{HotspotShare: 1, Focus: []string{"a"}})
		east = append(east, (lng-h.Lng)*metersPerDegreeLng(h.Lat))
		north = append(north, (lat-h.Lat)*metersPerDegreeLat)
	}
	for name, xs := range map[string][]float64{"east": east, "north": north} {
		m, sd := meanStd(xs)
		if math.Abs(m) > 20 || math.Abs(sd-h.SigmaM) > 0.1*h.SigmaM {
			t.Errorf("%s offset: mean %.0fm sd %.0fm, want about 0 and %.0f", name, m, sd, h.SigmaM)
		}
	}

	// Share 0 means uniform: the mean is the middle of the area.
	var lats, lngs []float64
	for i := 0; i < 20000; i++ {
		lat, lng := s.pickup(Phase{HotspotShare: 0})
		lats, lngs = append(lats, lat), append(lngs, lng)
	}
	ml, _ := meanStd(lats)
	mg, _ := meanStd(lngs)
	if math.Abs(ml-config.JakartaCenterLat) > 0.002 || math.Abs(mg-config.JakartaCenterLng) > 0.002 {
		t.Errorf("uniform mean %v,%v is not the centre of the area", ml, mg)
	}

	// Hotspots are chosen by weight (3:1) when no focus is set.
	hitA := 0
	for i := 0; i < 20000; i++ {
		lat, lng := s.pickup(Phase{HotspotShare: 1})
		if math.Hypot((lat-p.Hotspots[0].Lat)*metersPerDegreeLat, (lng-p.Hotspots[0].Lng)*metersPerDegreeLng(lat)) <
			math.Hypot((lat-p.Hotspots[1].Lat)*metersPerDegreeLat, (lng-p.Hotspots[1].Lng)*metersPerDegreeLng(lat)) {
			hitA++
		}
	}
	if share := float64(hitA) / 20000; share < 0.65 || share > 0.85 {
		t.Errorf("hotspot a got %.2f of orders, want about 0.75", share)
	}
}

func TestDropoffWithinRadius(t *testing.T) {
	p := testProfile()
	s := newSampler(p, rand.New(rand.NewSource(4)))
	for i := 0; i < 5000; i++ {
		lat, lng :=
			config.JakartaCenterLat, config.JakartaCenterLng // far from the edges, so no clamping
		dlat, dlng := s.dropoff(lat, lng)
		d := math.Hypot((dlat-lat)*metersPerDegreeLat, (dlng-lng)*metersPerDegreeLng(lat))
		if d > p.DropoffRadiusM*1.01 {
			t.Fatalf("dropoff %.0fm away, radius is %.0fm", d, p.DropoffRadiusM)
		}
	}
}

func newTestCourier(t *testing.T, mutate func(p *Profile)) *Courier {
	p := testProfile()
	if mutate != nil {
		mutate(p)
	}
	return newCourier("c", p, rand.New(rand.NewSource(5)))
}

func TestIdleCourierStaysInsideTheServiceArea(t *testing.T) {
	c := newTestCourier(t, nil)
	for i := 0; i < 20000; i++ {
		c.tick(3)
		if !inBBox(c.lat, c.lng) {
			t.Fatalf("courier left the service area at tick %d: %v,%v", i, c.lat, c.lng)
		}
	}
}

func TestIdleCourierMovesAtItsSpeed(t *testing.T) {
	c := newTestCourier(t, func(p *Profile) { p.SpeedMps = 10 })
	// Start away from the edges so no bounce interferes.
	c.lat, c.lng = config.JakartaCenterLat, config.JakartaCenterLng
	lat0, lng0 := c.lat, c.lng
	c.tick(2)
	d := math.Hypot((c.lat-lat0)*metersPerDegreeLat, (c.lng-lng0)*metersPerDegreeLng(lat0))
	want := 10 * idleSpeedFactor * 2
	if math.Abs(d-want) > 0.5 {
		t.Errorf("moved %.2fm in one tick, want about %.2fm", d, want)
	}
}

func TestAssignedCourierDrivesPickupThenDropoffAndCompletesOnce(t *testing.T) {
	c := newTestCourier(t, func(p *Profile) { p.SpeedMps = 500 })
	c.lat, c.lng = config.JakartaCenterLat, config.JakartaCenterLng
	loc := orderLocation{
		PickupLat: config.JakartaCenterLat + 0.01, PickupLng: config.JakartaCenterLng,
		DropoffLat: config.JakartaCenterLat + 0.01, DropoffLng: config.JakartaCenterLng + 0.01,
	}
	orderLocationsMu.Lock()
	orderLocations["o1"] = loc
	orderLocationsMu.Unlock()
	c.currentOrder, c.state = "o1", stateToPickup
	c.targetLat, c.targetLng = loc.PickupLat, loc.PickupLng

	completions := 0
	for i := 0; i < 200 && completions == 0; i++ {
		if c.tick(1) == "o1" {
			completions++
		}
	}
	if completions != 1 || c.state != stateIdle || c.currentOrder != "" {
		t.Fatalf("completions=%d state=%v order=%q", completions, c.state, c.currentOrder)
	}
	if c.lat != loc.DropoffLat || c.lng != loc.DropoffLng {
		t.Errorf("courier ended at %v,%v, want the dropoff", c.lat, c.lng)
	}
	for i := 0; i < 50; i++ {
		if c.tick(1) != "" {
			t.Fatal("completion must be reported exactly once")
		}
	}
	orderLocationsMu.Lock()
	_, leaked := orderLocations["o1"]
	orderLocationsMu.Unlock()
	if leaked {
		t.Error("a finished order's location should be forgotten")
	}
}

func TestFlakyCourierGoesSilentAndNormalOnesNeverDo(t *testing.T) {
	normal := newTestCourier(t, func(p *Profile) { p.Behavior.FlakyFraction = 0 })
	flaky := newTestCourier(t, func(p *Profile) {
		p.Behavior.FlakyFraction = 1
		p.Behavior.FlakyOffS = [2]float64{10, 10}
		p.Behavior.FlakyOnS = [2]float64{5, 5}
	})
	if flaky.flaky != true || normal.flaky != false {
		t.Fatal("flaky_fraction 1 and 0 must give flaky and normal couriers")
	}
	now := time.Now()
	silent, total := 0, 0
	for s := 0.0; s < 300; s += 1 {
		at := now.Add(secs(s))
		if normal.silent(at) {
			t.Fatalf("a normal courier went silent at %vs", s)
		}
		if flaky.silent(at) {
			silent++
		}
		total++
	}
	// 10s off in every 15s cycle: about two thirds of the time.
	if frac := float64(silent) / float64(total); frac < 0.55 || frac > 0.8 {
		t.Errorf("flaky courier silent %.2f of the time, want about 0.67", frac)
	}
}

func TestBehaviorDrawsAcceptProbabilityAndDelay(t *testing.T) {
	p := testProfile()
	p.Behavior.AcceptMean, p.Behavior.AcceptStdDev = 0.85, 0.1
	p.Behavior.ResponseDelayMs = [2]int{1000, 8000}
	var probs []float64
	for i := 0; i < 5000; i++ {
		c := newCourier("c", p, rand.New(rand.NewSource(int64(i))))
		if c.acceptProb < 0 || c.acceptProb > 1 {
			t.Fatalf("accept probability %v out of range", c.acceptProb)
		}
		probs = append(probs, c.acceptProb)
		if d := c.responseDelay(); d < time.Second || d > 8*time.Second {
			t.Fatalf("response delay %v outside [1s, 8s]", d)
		}
	}
	if m, _ := meanStd(probs); math.Abs(m-0.85) > 0.02 {
		t.Errorf("mean accept probability %.3f, want about 0.85", m)
	}
}

func TestLatencyHistogram(t *testing.T) {
	s := newStats()
	if s.latencyQuantileMs(0.99) != 0 {
		t.Error("an empty histogram reports 0")
	}
	for i := 0; i < 99; i++ {
		s.observeLatency(3 * time.Millisecond) // bucket [2,4)
	}
	s.observeLatency(900 * time.Millisecond) // bucket [512,1024)
	if got := s.latencyQuantileMs(0.5); got != 4 {
		t.Errorf("p50 = %v, want 4 (upper bound of the 3ms bucket)", got)
	}
	if got := s.latencyQuantileMs(1); got != 1024 {
		t.Errorf("max quantile = %v, want 1024", got)
	}
	if s.maxLatencyMs.Load() != 900 {
		t.Errorf("max = %d, want 900", s.maxLatencyMs.Load())
	}
}
