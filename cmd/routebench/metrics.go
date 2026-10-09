package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
)

// scrape fetches and parses a Prometheus text endpoint.
func scrape(ctx context.Context, client *http.Client, url string) (map[string]*dto.MetricFamily, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	var parser expfmt.TextParser
	return parser.TextToMetricFamilies(resp.Body)
}

// total sums a counter or gauge across all its label sets. A metric that has
// not been observed yet (a labelled counter with no children) is 0.
func total(fams map[string]*dto.MetricFamily, name string) float64 {
	f, ok := fams[name]
	if !ok {
		return 0
	}
	sum := 0.0
	for _, m := range f.Metric {
		switch {
		case m.Counter != nil:
			sum += m.Counter.GetValue()
		case m.Gauge != nil:
			sum += m.Gauge.GetValue()
		}
	}
	return sum
}

// histogramOf merges a histogram metric across label sets.
func histogramOf(fams map[string]*dto.MetricFamily, name string) *dto.Histogram {
	f, ok := fams[name]
	if !ok || len(f.Metric) == 0 || f.Metric[0].Histogram == nil {
		return nil
	}
	return f.Metric[0].Histogram
}

// histQuantile estimates quantile q from cumulative buckets by linear
// interpolation inside the bucket that holds it, as Prometheus does. It
// returns NaN for an empty histogram. The result is only as fine as the
// bucket bounds.
func histQuantile(h *dto.Histogram, q float64) float64 {
	if h == nil || h.GetSampleCount() == 0 {
		return math.NaN()
	}
	rank := q * float64(h.GetSampleCount())
	prevBound, prevCount := 0.0, 0.0
	for _, b := range h.Bucket {
		count := float64(b.GetCumulativeCount())
		if count >= rank {
			bound := b.GetUpperBound()
			if math.IsInf(bound, 1) || count == prevCount {
				return prevBound
			}
			return prevBound + (bound-prevBound)*(rank-prevCount)/(count-prevCount)
		}
		prevBound, prevCount = b.GetUpperBound(), count
	}
	return prevBound
}
