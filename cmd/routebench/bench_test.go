package main

import (
	"encoding/json"
	"encoding/xml"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"google.golang.org/protobuf/proto"
)

func hist(bounds []float64, cumulative []uint64) *dto.Histogram {
	h := &dto.Histogram{SampleCount: proto.Uint64(cumulative[len(cumulative)-1])}
	for i, b := range bounds {
		h.Bucket = append(h.Bucket, &dto.Bucket{UpperBound: proto.Float64(b), CumulativeCount: proto.Uint64(cumulative[i])})
	}
	return h
}

func TestHistQuantileInterpolatesInsideTheBucket(t *testing.T) {
	// 100 samples: 50 up to 0.1s, 40 more up to 0.5s, 10 more up to 1s.
	h := hist([]float64{0.1, 0.5, 1}, []uint64{50, 90, 100})
	cases := []struct{ q, want float64 }{
		{0.25, 0.05}, // half way into the first bucket
		{0.5, 0.1},   // the first bucket's top
		{0.7, 0.3},   // half way into the second bucket
		{0.99, 0.95}, // 9/10 into the last bucket
	}
	for _, c := range cases {
		if got := histQuantile(h, c.q); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("q=%v: got %v, want %v", c.q, got, c.want)
		}
	}
	if !math.IsNaN(histQuantile(nil, 0.5)) || !math.IsNaN(histQuantile(hist([]float64{1}, []uint64{0}), 0.5)) {
		t.Error("an empty histogram has no quantile")
	}
}

func TestQueueStability(t *testing.T) {
	mk := func(depths ...float64) []Sample {
		var ss []Sample
		for i, d := range depths {
			ss = append(ss, Sample{T: float64(i), QueueDepth: d})
		}
		return ss
	}
	// Nine samples over a 9 second load: thirds are samples 0-2, 3-5, 6-8.
	if _, stable := queueStability(mk(500, 600, 500, 600, 500, 600, 550, 600, 580), 9); !stable {
		t.Error("a flat queue is stable")
	}
	if thirds, stable := queueStability(mk(100, 200, 300, 1000, 2000, 3000, 6000, 8000, 9000), 9); stable {
		t.Errorf("a queue that keeps growing is not stable: %v", thirds)
	}
	if _, stable := queueStability(mk(0, 1, 2, 3, 4, 5, 20, 30, 40), 9); !stable {
		t.Error("growth that stays under 100 counts as stable")
	}
	if _, stable := queueStability(mk(1, 2), 9); stable {
		t.Error("too few samples cannot show stability")
	}
	// Samples outside the load period are ignored.
	ss := append(mk(10, 10, 10, 10, 10, 10, 10, 10, 10), Sample{T: 500, QueueDepth: 1e9}, Sample{T: -5, QueueDepth: 1e9})
	if _, stable := queueStability(ss, 9); !stable {
		t.Error("samples outside the load period must not count")
	}
}

func TestSecondsToDrain(t *testing.T) {
	ss := []Sample{{T: 5, AwaitingMatch: 900}, {T: 10, AwaitingMatch: 700}, {T: 15, AwaitingMatch: 200}, {T: 20, AwaitingMatch: 3}, {T: 25, AwaitingMatch: 0}}
	if d, ok := secondsToDrain(ss, 10, 5); !ok || d != 10 {
		t.Errorf("got %v, %v; want 10 seconds after the spike ended", d, ok)
	}
	if _, ok := secondsToDrain(ss[:3], 10, 5); ok {
		t.Error("a queue that never drained must say so")
	}
	if d, ok := secondsToDrain([]Sample{{T: 3, AwaitingMatch: 0}, {T: 12, AwaitingMatch: 0}}, 10, 5); !ok || d != 2 {
		t.Errorf("samples before the spike ended must not count: %v %v", d, ok)
	}
}

func TestPhaseEnd(t *testing.T) {
	var p profileInfo
	json.Unmarshal([]byte(`{"phases":[{"name":"warm-up","duration_s":30},{"name":"spike","duration_s":10},{"name":"recovery","duration_s":90}]}`), &p)
	if end, ok := p.phaseEndS("spike"); !ok || end != 40 {
		t.Errorf("spike ends at %v (%v), want 40", end, ok)
	}
	if p.loadDurationS() != 130 {
		t.Errorf("load lasts %v, want 130", p.loadDurationS())
	}
	if _, ok := p.phaseEndS("nope"); ok {
		t.Error("an unknown phase has no end")
	}
}

func resultFor(mutate func(r *Result)) *Result {
	r := &Result{}
	r.MatchRate = 0.99
	r.MatchLatencyS.P50, r.MatchLatencyS.P99 = 0.5, 1.5
	r.QueueStable = true
	r.Batches.MaxSize = 100
	r.SurgePeak = 3
	if mutate != nil {
		mutate(r)
	}
	return r
}

func verdicts(ts []Target) map[string]string {
	out := map[string]string{}
	for _, t := range ts {
		switch {
		case t.Pass == nil:
			out[t.Name] = "reported"
		case *t.Pass:
			out[t.Name] = "pass"
		default:
			out[t.Name] = "miss"
		}
	}
	return out
}

func TestTargetsFollowTheSpecTable(t *testing.T) {
	// Baseline: p99 < 3s, match rate > 98%, zero sheds.
	v := verdicts(targetsFor("baseline", resultFor(nil)))
	if v["match latency p99"] != "pass" || v["match rate"] != "pass" || v["pings shed"] != "pass" || v["no double booking"] != "pass" {
		t.Errorf("a healthy baseline should pass: %v", v)
	}
	v = verdicts(targetsFor("baseline", resultFor(func(r *Result) {
		r.MatchLatencyS.P99 = 3.5
		r.MatchRate = 0.97
		r.Counters.PingsShed = 12
		r.DoubleBooked = 1
	})))
	for _, name := range []string{"match latency p99", "match rate", "pings shed", "no double booking"} {
		if v[name] != "miss" {
			t.Errorf("baseline %s should miss: %v", name, v)
		}
	}

	// Throughput has looser limits than baseline: 3.5s passes p99 < 4s.
	if v := verdicts(targetsFor("throughput", resultFor(func(r *Result) { r.MatchLatencyS.P99 = 3.5; r.MatchRate = 0.96 }))); v["match latency p99"] != "pass" || v["match rate"] != "pass" {
		t.Errorf("throughput limits are p99 < 4s and rate > 95%%: %v", v)
	}
	if v := verdicts(targetsFor("throughput", resultFor(func(r *Result) { r.QueueStable = false }))); v["queue depth stable"] != "miss" {
		t.Errorf("an unstable queue misses: %v", v)
	}

	// Ping flood: sheds are reported, not judged.
	v = verdicts(targetsFor("ping-flood", resultFor(func(r *Result) { r.Counters.PingsShed = 5000; r.Counters.PingIndexLagP99 = 0.4 })))
	if v["pings shed"] != "reported" || v["ping index lag p99"] != "pass" {
		t.Errorf("ping flood: %v", v)
	}
	if v := verdicts(targetsFor("ping-flood", resultFor(func(r *Result) { r.Counters.PingIndexLagP99 = 1.2 }))); v["ping index lag p99"] != "miss" {
		t.Errorf("a 1.2s index lag misses: %v", v)
	}

	// Hotspot spike.
	drain := 45.0
	v = verdicts(targetsFor("hotspot-spike", resultFor(func(r *Result) { r.DrainAfterSpik = &drain })))
	if v["batch size cap holds"] != "pass" || v["queue drains after the spike"] != "pass" || v["surge reaches the maximum"] != "pass" {
		t.Errorf("hotspot spike: %v", v)
	}
	late := 75.0
	v = verdicts(targetsFor("hotspot-spike", resultFor(func(r *Result) { r.DrainAfterSpik = &late; r.SurgePeak = 1.5; r.Batches.MaxSize = 101 })))
	for _, name := range []string{"batch size cap holds", "queue drains after the spike", "surge reaches the maximum"} {
		if v[name] != "miss" {
			t.Errorf("hotspot spike %s should miss: %v", name, v)
		}
	}
	if v := verdicts(targetsFor("hotspot-spike", resultFor(nil))); v["queue drains after the spike"] != "miss" {
		t.Errorf("a queue that never drained misses: %v", v)
	}

	// Contention: exhausted must be non-zero (the scenario is under-supplied on purpose).
	if v := verdicts(targetsFor("contention", resultFor(func(r *Result) { r.Orders.Exhausted = 4000; r.Orders.Created = 10000 }))); v["exhausted orders"] != "pass" {
		t.Errorf("contention with exhausted orders passes: %v", v)
	}
	if v := verdicts(targetsFor("contention", resultFor(nil))); v["exhausted orders"] != "miss" {
		t.Errorf("a contention run with no exhausted orders did not create contention: %v", v)
	}
}

func TestChartsAreWellFormedSVG(t *testing.T) {
	line := lineChart("a <title> & more", "seconds", "orders", []series{
		{"one", [][2]float64{{0, 0}, {10, 500}, {20, 300}}},
		{"two", nil}, // an empty series must not break the chart
	})
	bar := barChart("by window", "meters", []string{"0 ms", "2000 ms"}, []barGroup{
		{"distance", []float64{1200, 950}}, {"nan", []float64{math.NaN(), 5}},
	})
	for name, doc := range map[string]string{"line": line, "bar": bar} {
		if err := xml.NewDecoder(strings.NewReader(doc)).Decode(new(interface{})); err != nil {
			t.Errorf("%s chart is not valid XML: %v", name, err)
		}
		dec := xml.NewDecoder(strings.NewReader(doc))
		for {
			if _, err := dec.Token(); err != nil {
				if err.Error() != "EOF" {
					t.Errorf("%s chart: %v", name, err)
				}
				break
			}
		}
		if strings.Contains(doc, "NaN") || strings.Contains(doc, "Inf") {
			t.Errorf("%s chart contains a non-finite coordinate", name)
		}
	}
	if !strings.Contains(line, "a &lt;title&gt; &amp; more") {
		t.Error("chart titles must be escaped")
	}
	if niceMax(0) != 1 || niceMax(437) != 500 || niceMax(1) != 1 || niceMax(2.1) != 5 || niceMax(math.NaN()) != 1 {
		t.Errorf("niceMax: %v %v %v %v", niceMax(0), niceMax(437), niceMax(1), niceMax(2.1))
	}
}

func TestCompareBuildsTheTable(t *testing.T) {
	dir := t.TempDir()
	write := func(window int, pickup, p50, p99 float64) {
		var r Result
		r.Label, r.BatchWindowMs, r.PickupMeanM = "w", window, pickup
		r.MatchLatencyS.P50, r.MatchLatencyS.P99 = p50, p99
		r.Orders.Created, r.MatchRate = 5000, 0.99
		raw, _ := json.Marshal(r)
		os.WriteFile(filepath.Join(dir, "solver-comparison-window-"+strconv.Itoa(window)+"ms.json"), raw, 0o644)
	}
	write(0, 1000, 0.2, 0.5)
	write(2000, 900, 1.2, 2.5)
	write(4000, 850, 2.2, 4.5)
	if err := runCompare(dir); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "solver-comparison.json"))
	var rows []compareRow
	json.Unmarshal(raw, &rows)
	if len(rows) != 3 || rows[1].DistanceVsZero != -10 || rows[2].DistanceVsZero != -15 {
		t.Fatalf("rows: %+v", rows)
	}
	if math.Abs(rows[1].AddedLatencyS-1.0) > 1e-9 {
		t.Errorf("added latency for 2000 ms = %v, want 1.0", rows[1].AddedLatencyS)
	}
	for _, f := range []string{"solver-comparison.md", "solver-comparison-distance.svg", "solver-comparison-latency.svg"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// No baseline, no comparison.
	os.Remove(filepath.Join(dir, "solver-comparison-window-0ms.json"))
	if err := runCompare(dir); err == nil {
		t.Error("a comparison without the 0 ms baseline must fail")
	}
}

func TestSummaryListsTargetsAndCounts(t *testing.T) {
	dir := t.TempDir()
	var r Result
	r.Scenario, r.Hardware = "baseline", "test rig"
	r.Targets = []Target{
		{"match rate", "> 98%", "99.00%", ptr(true)},
		{"pings shed", "0", "12", ptr(false)},
		{"queue", "reported", "5", nil},
	}
	raw, _ := json.Marshal(r)
	os.WriteFile(filepath.Join(dir, "baseline.json"), raw, 0o644)
	if err := runSummary(dir); err != nil {
		t.Fatal(err)
	}
	md, _ := os.ReadFile(filepath.Join(dir, "SUMMARY.md"))
	text := string(md)
	for _, want := range []string{"1 targets met, 1 missed", "test rig", "| match rate | > 98% | 99.00% | met |", "| pings shed | 0 | 12 | **missed** |", "| queue | reported | 5 | reported |", "baseline.svg"} {
		if !strings.Contains(text, want) {
			t.Errorf("summary is missing %q:\n%s", want, text)
		}
	}
	if err := runSummary(t.TempDir()); err == nil {
		t.Error("an empty folder has nothing to summarise")
	}
}
