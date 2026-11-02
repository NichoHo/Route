package metrics

import (
	"math"
	"testing"
	"time"
)

func inf() float64 { return math.Inf(1) }

func TestQuantileInterpolatesInsideTheBucket(t *testing.T) {
	// 100 observations: 50 up to 1s, 40 more up to 2s, 10 more up to 5s.
	b := []Bucket{{1, 50}, {2, 90}, {5, 100}, {inf(), 100}}
	if got, _ := quantile(b, 0.5); math.Abs(got-1) > 1e-9 {
		t.Errorf("p50 = %v, want 1", got)
	}
	if got, _ := quantile(b, 0.7); math.Abs(got-1.5) > 1e-9 {
		t.Errorf("p70 = %v, want 1.5", got)
	}
	if got, _ := quantile(b, 0.99); math.Abs(got-4.7) > 1e-9 {
		t.Errorf("p99 = %v, want 4.7", got)
	}
}

func TestQuantileBeyondTheLastBoundReturnsThatBound(t *testing.T) {
	b := []Bucket{{10, 1}, {30, 1}, {inf(), 10}}
	if got, _ := quantile(b, 0.99); got != 30 {
		t.Errorf("p99 = %v, want the last finite bound 30", got)
	}
}

func TestQuantileOfNothing(t *testing.T) {
	if _, ok := quantile([]Bucket{{1, 0}, {inf(), 0}}, 0.5); ok {
		t.Error("an empty histogram has no quantile")
	}
}

func TestBetweenGivesRatesAndPercentiles(t *testing.T) {
	t0 := time.Now()
	a := Sample{At: t0, Orders: 100, Accepted: 10, Exhausted: 0, PingsShed: 0,
		Latency: []Bucket{{1, 5}, {2, 5}, {inf(), 5}}}
	b := Sample{At: t0.Add(10 * time.Second), Orders: 600, Accepted: 90, Exhausted: 20, PingsShed: 50,
		BatchRatio: 0.92, QueueDepth: 7,
		Latency: []Bucket{{1, 55}, {2, 105}, {inf(), 105}}}
	l := Between(a, b)
	if l.OrdersPerS != 50 {
		t.Errorf("orders/s = %v, want 50", l.OrdersPerS)
	}
	if l.PingsShedPerS != 5 {
		t.Errorf("pings shed/s = %v, want 5", l.PingsShedPerS)
	}
	if l.MatchRate == nil || math.Abs(*l.MatchRate-0.8) > 1e-9 {
		t.Errorf("match rate = %v, want 0.8 (80 assigned, 20 exhausted)", l.MatchRate)
	}
	if l.BatchGainPct == nil || math.Abs(*l.BatchGainPct-8) > 1e-9 {
		t.Errorf("batch gain = %v, want 8", l.BatchGainPct)
	}
	if l.MatchP50S == nil || math.Abs(*l.MatchP50S-1) > 1e-9 {
		t.Errorf("p50 = %v, want 1", l.MatchP50S)
	}
	if l.QueueDepth != 7 {
		t.Errorf("queue depth = %v, want 7", l.QueueDepth)
	}
}

func TestBetweenWithNothingToSayLeavesPointersNil(t *testing.T) {
	t0 := time.Now()
	s := Sample{At: t0, Latency: []Bucket{{1, 0}, {inf(), 0}}}
	l := Between(s, Sample{At: t0.Add(time.Second), Latency: []Bucket{{1, 0}, {inf(), 0}}})
	if l.MatchRate != nil || l.MatchP50S != nil || l.MatchP99S != nil || l.BatchGainPct != nil {
		t.Errorf("expected nil for everything that has no data: %+v", l)
	}
}

func TestWindowDropsOldReadings(t *testing.T) {
	var w Window
	t0 := time.Now()
	w.Add(Sample{At: t0, Orders: 0})
	// 60s later: the first reading is outside the window, so there is no rate
	// to report until the next reading arrives.
	if l := w.Add(Sample{At: t0.Add(60 * time.Second), Orders: 600}); l.OrdersPerS != 0 {
		t.Errorf("orders/s = %v after a long gap, want 0", l.OrdersPerS)
	}
	if l := w.Add(Sample{At: t0.Add(62 * time.Second), Orders: 700}); l.OrdersPerS != 50 {
		t.Errorf("orders/s = %v, want 50", l.OrdersPerS)
	}
}
