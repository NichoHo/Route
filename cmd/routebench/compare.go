package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// compareRow is one batch window's outcome.
type compareRow struct {
	WindowMs        int     `json:"batch_window_ms"`
	Orders          int     `json:"orders"`
	MeanPickupM     float64 `json:"mean_pickup_distance_m"`
	LatencyP50S     float64 `json:"match_latency_p50_s"`
	LatencyP99S     float64 `json:"match_latency_p99_s"`
	MatchRate       float64 `json:"match_rate"`
	DistanceVsZero  float64 `json:"pickup_distance_change_vs_0ms_pct"` // negative is shorter pickups
	AddedLatencyS   float64 `json:"added_p50_latency_vs_0ms_s"`
	BatchedOverGrdy float64 `json:"batched_over_greedy_distance_in_batch"`
}

// runCompare reads the solver-comparison-window-*ms.json runs and writes the
// chart that justifies (or fails to justify) batching: pickup distance saved
// against latency added, per window length.
func runCompare(dir string) error {
	files, _ := filepath.Glob(filepath.Join(dir, "solver-comparison-window-*ms.json"))
	var rows []compareRow
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		var r Result
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("%s: %w", f, err)
		}
		rows = append(rows, compareRow{
			WindowMs: r.BatchWindowMs, Orders: r.Orders.Created, MeanPickupM: r.PickupMeanM,
			LatencyP50S: r.MatchLatencyS.P50, LatencyP99S: r.MatchLatencyS.P99, MatchRate: r.MatchRate,
			BatchedOverGrdy: r.Batches.DistanceRatio,
		})
	}
	if len(rows) == 0 {
		return fmt.Errorf("no solver-comparison-window-*ms.json files in %s", dir)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].WindowMs < rows[j].WindowMs })
	if rows[0].WindowMs != 0 {
		return fmt.Errorf("the 0 ms run is the baseline and is missing")
	}
	base := rows[0]
	for i := range rows {
		if base.MeanPickupM > 0 {
			rows[i].DistanceVsZero = 100 * (rows[i].MeanPickupM - base.MeanPickupM) / base.MeanPickupM
		}
		rows[i].AddedLatencyS = rows[i].LatencyP50S - base.LatencyP50S
	}

	cats := make([]string, len(rows))
	dist := barGroup{Name: "mean pickup distance (m)"}
	p50 := barGroup{Name: "match latency p50 (s)"}
	p99 := barGroup{Name: "match latency p99 (s)"}
	for i, r := range rows {
		cats[i] = fmt.Sprintf("%d ms", r.WindowMs)
		dist.Values = append(dist.Values, r.MeanPickupM)
		p50.Values = append(p50.Values, r.LatencyP50S)
		p99.Values = append(p99.Values, r.LatencyP99S)
	}
	// Two standalone files are easier to embed than one with two roots.
	if err := os.WriteFile(filepath.Join(dir, "solver-comparison-distance.svg"),
		[]byte(barChart("Pickup distance by batch window", "meters", cats, []barGroup{dist})), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "solver-comparison-latency.svg"),
		[]byte(barChart("Match latency by batch window", "seconds", cats, []barGroup{p50, p99})), 0o644); err != nil {
		return err
	}

	out, _ := json.MarshalIndent(rows, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "solver-comparison.json"), out, 0o644); err != nil {
		return err
	}

	var md strings.Builder
	md.WriteString("| Batch window | Orders | Mean pickup (m) | vs 0 ms | Match latency p50 | p99 | Added p50 latency | Match rate |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range rows {
		change := "baseline"
		if r.WindowMs != 0 {
			change = fmt.Sprintf("%+.1f%%", r.DistanceVsZero)
		}
		fmt.Fprintf(&md, "| %d ms | %d | %.0f | %s | %.2f s | %.2f s | %+.2f s | %.1f%% |\n",
			r.WindowMs, r.Orders, r.MeanPickupM, change, r.LatencyP50S, r.LatencyP99S, r.AddedLatencyS, r.MatchRate*100)
	}
	if err := os.WriteFile(filepath.Join(dir, "solver-comparison.md"), []byte(md.String()), 0o644); err != nil {
		return err
	}
	fmt.Print(md.String())
	return nil
}
