package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"time"

	"route/internal/config"
)

// Profile is a scenario: how many couriers there are, how they behave, and
// how orders arrive over time (ROUTE_SPEC.md section 6.8). The load tests in
// section 11 are committed profiles under loadtest/scenarios.
type Profile struct {
	Name        string `json:"name"`
	Description string `json:"description"`

	Couriers       int     `json:"couriers"`
	RampUpS        float64 `json:"ramp_up_s"`        // spread courier connections over this long
	PingIntervalMs int     `json:"ping_interval_ms"` // spec default 3000
	// SpeedMps is how fast an assigned courier drives. It is deliberately
	// high: the simulation compresses time so supply turns over at benchmark
	// order rates (50 orders/s with 1000 couriers needs deliveries of seconds,
	// not minutes). See docs/decisions/008-simulator-first-class.md.
	SpeedMps       float64 `json:"speed_mps"`
	DropoffRadiusM float64 `json:"dropoff_radius_m"`
	Workers        int     `json:"workers"` // concurrent HTTP clients posting orders
	DrainS         float64 `json:"drain_s"` // keep couriers running this long after the last phase
	Loop           bool    `json:"loop"`

	Behavior Behavior  `json:"behavior"`
	Hotspots []Hotspot `json:"hotspots"`
	Phases   []Phase   `json:"phases"`
}

// Behavior is how couriers respond. Each courier draws its own accept
// probability from a normal distribution around AcceptMean.
type Behavior struct {
	AcceptMean      float64    `json:"accept_mean"`
	AcceptStdDev    float64    `json:"accept_stddev"`
	ResponseDelayMs [2]int     `json:"response_delay_ms"` // uniform [min, max]
	FlakyFraction   float64    `json:"flaky_fraction"`    // share of couriers that go silent now and then
	FlakyOffS       [2]float64 `json:"flaky_off_s"`       // how long a flaky courier stays silent
	FlakyOnS        [2]float64 `json:"flaky_on_s"`        // how long it pings between outages
}

// Hotspot is a place orders cluster around, with Gaussian spread.
type Hotspot struct {
	Name   string  `json:"name"`
	Lat    float64 `json:"lat"`
	Lng    float64 `json:"lng"`
	SigmaM float64 `json:"sigma_m"`
	Weight float64 `json:"weight"`
}

// Phase is one stretch of the scenario. Orders arrive as a Poisson process at
// OrdersPerS. A share HotspotShare of them is drawn from the hotspots
// (restricted to Focus if it is set); the rest are uniform over the service
// area.
type Phase struct {
	Name         string   `json:"name"`
	DurationS    float64  `json:"duration_s"`
	OrdersPerS   float64  `json:"orders_per_s"`
	HotspotShare float64  `json:"hotspot_share"`
	Focus        []string `json:"focus"`
}

// LoadProfile reads, defaults and validates a profile file. Unknown fields
// are an error, so a typo in a scenario file does not silently change it.
func LoadProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.applyDefaults()
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}

func (p *Profile) applyDefaults() {
	if p.Couriers == 0 {
		p.Couriers = 50
	}
	if p.PingIntervalMs == 0 {
		p.PingIntervalMs = 3000
	}
	if p.SpeedMps == 0 {
		p.SpeedMps = 60
	}
	if p.DropoffRadiusM == 0 {
		p.DropoffRadiusM = 2000
	}
	if p.Workers == 0 {
		p.Workers = 64
	}
	b := &p.Behavior
	if b.AcceptMean == 0 {
		b.AcceptMean = 0.85
	}
	if b.AcceptStdDev == 0 {
		b.AcceptStdDev = 0.08
	}
	if b.ResponseDelayMs == [2]int{} {
		b.ResponseDelayMs = [2]int{1000, 8000}
	}
	if b.FlakyOffS == [2]float64{} {
		b.FlakyOffS = [2]float64{20, 60}
	}
	if b.FlakyOnS == [2]float64{} {
		b.FlakyOnS = [2]float64{30, 120}
	}
}

func (p *Profile) validate() error {
	if p.Name == "" {
		return fmt.Errorf("name is required")
	}
	if p.Couriers < 1 {
		return fmt.Errorf("couriers must be at least 1")
	}
	if p.PingIntervalMs < 100 {
		return fmt.Errorf("ping_interval_ms must be at least 100")
	}
	if p.SpeedMps <= 0 || p.DropoffRadiusM <= 0 || p.Workers < 1 || p.RampUpS < 0 || p.DrainS < 0 {
		return fmt.Errorf("speed_mps, dropoff_radius_m, workers must be positive; ramp_up_s, drain_s must not be negative")
	}
	b := p.Behavior
	if b.AcceptMean < 0 || b.AcceptMean > 1 || b.AcceptStdDev < 0 || b.FlakyFraction < 0 || b.FlakyFraction > 1 {
		return fmt.Errorf("behavior: accept_mean and flaky_fraction must be in [0,1], accept_stddev not negative")
	}
	if b.ResponseDelayMs[0] < 0 || b.ResponseDelayMs[0] > b.ResponseDelayMs[1] {
		return fmt.Errorf("behavior: response_delay_ms must be [min, max] with 0 <= min <= max")
	}
	if b.FlakyOffS[0] <= 0 || b.FlakyOffS[0] > b.FlakyOffS[1] || b.FlakyOnS[0] <= 0 || b.FlakyOnS[0] > b.FlakyOnS[1] {
		return fmt.Errorf("behavior: flaky_off_s and flaky_on_s must be [min, max] with 0 < min <= max")
	}

	names := map[string]bool{}
	for _, h := range p.Hotspots {
		if h.Name == "" || h.SigmaM <= 0 || h.Weight <= 0 {
			return fmt.Errorf("hotspot %q: name, sigma_m and weight are required and positive", h.Name)
		}
		if !inBBox(h.Lat, h.Lng) {
			return fmt.Errorf("hotspot %q is outside the service area", h.Name)
		}
		if names[h.Name] {
			return fmt.Errorf("duplicate hotspot name %q", h.Name)
		}
		names[h.Name] = true
	}
	if len(p.Phases) == 0 {
		return fmt.Errorf("at least one phase is required")
	}
	for i, ph := range p.Phases {
		if ph.DurationS <= 0 || ph.OrdersPerS < 0 || ph.HotspotShare < 0 || ph.HotspotShare > 1 {
			return fmt.Errorf("phase %d (%s): duration_s must be positive, orders_per_s not negative, hotspot_share in [0,1]", i, ph.Name)
		}
		if ph.HotspotShare > 0 && len(p.Hotspots) == 0 {
			return fmt.Errorf("phase %d (%s) uses hotspots but the profile has none", i, ph.Name)
		}
		for _, f := range ph.Focus {
			if !names[f] {
				return fmt.Errorf("phase %d (%s) focuses on unknown hotspot %q", i, ph.Name, f)
			}
		}
	}
	return nil
}

// totalDurationS is the length of one pass through the phases.
func (p *Profile) totalDurationS() float64 {
	t := 0.0
	for _, ph := range p.Phases {
		t += ph.DurationS
	}
	return t
}

// PhaseAt returns the phase in effect after elapsed time, and false once a
// non-looping scenario has ended.
func (p *Profile) PhaseAt(elapsed time.Duration) (Phase, bool) {
	t := elapsed.Seconds()
	total := p.totalDurationS()
	if p.Loop {
		t = math.Mod(t, total)
	} else if t >= total {
		return Phase{}, false
	}
	for _, ph := range p.Phases {
		if t < ph.DurationS {
			return ph, true
		}
		t -= ph.DurationS
	}
	return p.Phases[len(p.Phases)-1], true
}

// defaultProfile is what runs when SIM_PROFILE is not set: the original demo
// load, tunable with SIM_COURIERS and SIM_ORDER_INTERVAL.
func defaultProfile(couriers int, orderInterval time.Duration) *Profile {
	p := &Profile{
		Name:           "default",
		Description:    "Uniform demo load from SIM_COURIERS and SIM_ORDER_INTERVAL.",
		Couriers:       couriers,
		PingIntervalMs: 1000,
		Behavior:       Behavior{ResponseDelayMs: [2]int{1000, 3000}},
		Phases:         []Phase{{Name: "steady", DurationS: 3600, OrdersPerS: 1 / orderInterval.Seconds()}},
		Loop:           true,
	}
	p.applyDefaults()
	return p
}

// ---- sampling (pure given the rng) ----

const metersPerDegreeLat = 111320.0

func metersPerDegreeLng(lat float64) float64 {
	return metersPerDegreeLat * math.Cos(lat*math.Pi/180)
}

func inBBox(lat, lng float64) bool {
	return lat >= config.JakartaMinLat && lat <= config.JakartaMaxLat &&
		lng >= config.JakartaMinLng && lng <= config.JakartaMaxLng
}

func clampToBBox(lat, lng float64) (float64, float64) {
	return math.Min(math.Max(lat, config.JakartaMinLat), config.JakartaMaxLat),
		math.Min(math.Max(lng, config.JakartaMinLng), config.JakartaMaxLng)
}

func uniformPoint(rng *rand.Rand) (lat, lng float64) {
	return config.JakartaMinLat + rng.Float64()*(config.JakartaMaxLat-config.JakartaMinLat),
		config.JakartaMinLng + rng.Float64()*(config.JakartaMaxLng-config.JakartaMinLng)
}

// sampler draws order locations for a profile.
type sampler struct {
	rng      *rand.Rand
	hotspots []Hotspot
	radiusM  float64
}

func newSampler(p *Profile, rng *rand.Rand) *sampler {
	return &sampler{rng: rng, hotspots: p.Hotspots, radiusM: p.DropoffRadiusM}
}

// pickup draws a pickup point for phase ph. A point that falls outside the
// service area (a hotspot near the edge) is drawn again, and clamped after a
// few tries.
func (s *sampler) pickup(ph Phase) (lat, lng float64) {
	if ph.HotspotShare > 0 && s.rng.Float64() < ph.HotspotShare {
		if h, ok := s.pickHotspot(ph.Focus); ok {
			for try := 0; try < 8; try++ {
				lat = h.Lat + s.rng.NormFloat64()*h.SigmaM/metersPerDegreeLat
				lng = h.Lng + s.rng.NormFloat64()*h.SigmaM/metersPerDegreeLng(h.Lat)
				if inBBox(lat, lng) {
					return lat, lng
				}
			}
			return clampToBBox(lat, lng)
		}
	}
	return uniformPoint(s.rng)
}

// pickHotspot chooses a hotspot by weight, among focus if it is not empty.
func (s *sampler) pickHotspot(focus []string) (Hotspot, bool) {
	var pool []Hotspot
	total := 0.0
	for _, h := range s.hotspots {
		if len(focus) > 0 && !contains(focus, h.Name) {
			continue
		}
		pool = append(pool, h)
		total += h.Weight
	}
	if len(pool) == 0 {
		return Hotspot{}, false
	}
	r := s.rng.Float64() * total
	for _, h := range pool {
		if r < h.Weight {
			return h, true
		}
		r -= h.Weight
	}
	return pool[len(pool)-1], true
}

// dropoff is the pickup plus a random offset within the dropoff radius,
// clamped to the service area.
func (s *sampler) dropoff(lat, lng float64) (float64, float64) {
	angle := s.rng.Float64() * 2 * math.Pi
	r := math.Sqrt(s.rng.Float64()) * s.radiusM
	return clampToBBox(lat+r*math.Cos(angle)/metersPerDegreeLat, lng+r*math.Sin(angle)/metersPerDegreeLng(lat))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// poisson draws the number of arrivals in an interval whose expected count is
// mean. Knuth's method is exact but slow for large means, so those use the
// normal approximation.
func poisson(rng *rand.Rand, mean float64) int {
	if mean <= 0 {
		return 0
	}
	if mean > 30 {
		n := int(math.Round(mean + math.Sqrt(mean)*rng.NormFloat64()))
		if n < 0 {
			return 0
		}
		return n
	}
	limit, k, p := math.Exp(-mean), 0, 1.0
	for {
		p *= rng.Float64()
		if p <= limit {
			return k
		}
		k++
	}
}

// uniformIn draws uniformly from [lo, hi].
func uniformIn(rng *rand.Rand, lo, hi float64) float64 {
	return lo + rng.Float64()*(hi-lo)
}
