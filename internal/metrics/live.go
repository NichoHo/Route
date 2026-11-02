package metrics

import (
	"math"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// liveWindow is how far back the dashboard's rates and percentiles look.
const liveWindow = 10 * time.Second

// Bucket is one cumulative bucket of a histogram: how many observations were
// at or below Upper.
type Bucket struct {
	Upper float64
	Count uint64
}

// Sample is one reading of the numbers behind the dashboard's metrics strip.
type Sample struct {
	At        time.Time
	Orders    float64 // orders created
	Accepted  float64 // offers accepted, which is orders assigned
	Exhausted float64 // orders given up on
	PingsShed float64
	Latency   []Bucket // match latency, cumulative, ends with +Inf

	// Gauges, read as they are.
	BatchRatio float64 // batched / greedy pickup distance; 0 before any batch
	QueueDepth float64
}

// Live is what the dashboard shows. A pointer is nil when there is nothing to
// say yet, for example no matches in the window.
type Live struct {
	OrdersPerS    float64  `json:"orders_per_s"`
	MatchP50S     *float64 `json:"match_p50_s"`
	MatchP99S     *float64 `json:"match_p99_s"`
	MatchRate     *float64 `json:"match_rate"`     // assigned / (assigned + exhausted), 0 to 1
	BatchGainPct  *float64 `json:"batch_gain_pct"` // how much shorter batched pickups are than greedy
	PingsShedPerS float64  `json:"pings_shed_per_s"`
	QueueDepth    float64  `json:"queue_depth"`
}

func gaugeVal(g prometheus.Gauge) float64 {
	var m dto.Metric
	if g.Write(&m) != nil {
		return 0
	}
	return m.GetGauge().GetValue()
}

// Take reads the current values.
func Take() Sample {
	s := Sample{
		At:         time.Now(),
		Orders:     counterValue(OrdersCreatedTotal),
		Accepted:   counterValue(OfferOutcomeTotal.WithLabelValues("accepted")),
		Exhausted:  counterValue(OrdersExhaustedTotal),
		PingsShed:  counterValue(PingsShedTotal),
		BatchRatio: gaugeVal(BatchedVsGreedyRatio),
		QueueDepth: gaugeVal(QueueDepth),
	}
	var m dto.Metric
	if MatchLatencySeconds.Write(&m) == nil {
		h := m.GetHistogram()
		for _, b := range h.GetBucket() {
			s.Latency = append(s.Latency, Bucket{b.GetUpperBound(), b.GetCumulativeCount()})
		}
		s.Latency = append(s.Latency, Bucket{math.Inf(1), h.GetSampleCount()})
	}
	return s
}

// Between turns two readings into rates and percentiles over the time between
// them. Gauges come from the later reading.
func Between(a, b Sample) Live {
	l := Live{QueueDepth: b.QueueDepth}
	if b.BatchRatio > 0 {
		g := (1 - b.BatchRatio) * 100
		l.BatchGainPct = &g
	}
	secs := b.At.Sub(a.At).Seconds()
	if secs <= 0 {
		return l
	}
	l.OrdersPerS = math.Max(0, b.Orders-a.Orders) / secs
	l.PingsShedPerS = math.Max(0, b.PingsShed-a.PingsShed) / secs
	if done := (b.Accepted - a.Accepted) + (b.Exhausted - a.Exhausted); done > 0 {
		r := (b.Accepted - a.Accepted) / done
		l.MatchRate = &r
	}
	if len(a.Latency) == len(b.Latency) {
		delta := make([]Bucket, len(b.Latency))
		for i := range delta {
			delta[i] = Bucket{b.Latency[i].Upper, b.Latency[i].Count - a.Latency[i].Count}
		}
		if p50, ok := quantile(delta, 0.5); ok {
			l.MatchP50S = &p50
			p99, _ := quantile(delta, 0.99)
			l.MatchP99S = &p99
		}
	}
	return l
}

// quantile estimates the q-th quantile of a cumulative histogram by straight
// interpolation inside the bucket that holds it. Beyond the last finite bound
// it returns that bound. ok is false when the histogram is empty.
func quantile(buckets []Bucket, q float64) (float64, bool) {
	if len(buckets) == 0 {
		return 0, false
	}
	total := buckets[len(buckets)-1].Count
	if total == 0 {
		return 0, false
	}
	target := q * float64(total)
	lowerBound, lowerCount := 0.0, uint64(0)
	for _, b := range buckets {
		if float64(b.Count) >= target {
			if math.IsInf(b.Upper, 1) {
				return lowerBound, true
			}
			if b.Count == lowerCount {
				return b.Upper, true
			}
			frac := (target - float64(lowerCount)) / float64(b.Count-lowerCount)
			return lowerBound + (b.Upper-lowerBound)*frac, true
		}
		lowerBound, lowerCount = b.Upper, b.Count
	}
	return lowerBound, true
}

// Window keeps the last few seconds of readings. Each Add returns the live
// numbers over that stretch.
type Window struct {
	mu      sync.Mutex
	samples []Sample
}

// Add records s and returns the numbers over the readings that are still
// within the window. With one reading there are no rates yet.
func (w *Window) Add(s Sample) Live {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.samples = append(w.samples, s)
	keep := 0
	for keep < len(w.samples)-1 && s.At.Sub(w.samples[keep].At) > liveWindow {
		keep++
	}
	w.samples = w.samples[keep:]
	return Between(w.samples[0], s)
}
