package matcher

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

func randomMatrix(rng *rand.Rand, n, m int) [][]float64 {
	cm := make([][]float64, n)
	for i := range cm {
		cm[i] = make([]float64, m)
		for j := range cm[i] {
			cm[i][j] = rng.Float64() * 8000
		}
	}
	return cm
}

// bruteForceMin returns the minimum total cost over all ways to assign
// min(n, m) rows to distinct columns, leaving surplus rows unassigned.
func bruteForceMin(cm [][]float64) float64 {
	n, m := len(cm), len(cm[0])
	best := math.MaxFloat64
	usedCol := make([]bool, m)
	var rec func(row, assigned int, cost float64)
	rec = func(row, assigned int, cost float64) {
		need := n
		if m < n {
			need = m
		}
		if assigned == need {
			if cost < best {
				best = cost
			}
			return
		}
		if row == n {
			return
		}
		for j := 0; j < m; j++ {
			if !usedCol[j] {
				usedCol[j] = true
				rec(row+1, assigned+1, cost+cm[row][j])
				usedCol[j] = false
			}
		}
		if n-row-1 >= need-assigned { // this row may stay unassigned
			rec(row+1, assigned, cost)
		}
	}
	rec(0, 0, 0)
	return best
}

// The solver must be optimal for wide, tall and square matrices.
func TestHungarianMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for run := 0; run < 300; run++ {
		n, m := 1+rng.Intn(6), 1+rng.Intn(6)
		cm := randomMatrix(rng, n, m)

		got := SolveHungarian(cm)
		total, seen, matched := 0.0, map[int]bool{}, 0
		for i, col := range got {
			if col == -1 {
				continue
			}
			if seen[col] {
				t.Fatalf("column %d assigned twice in %v", col, got)
			}
			seen[col] = true
			total += cm[i][col]
			matched++
		}
		want := n
		if m < n {
			want = m
		}
		if matched != want {
			t.Fatalf("%dx%d: matched %d, want %d", n, m, matched, want)
		}
		if opt := bruteForceMin(cm); math.Abs(total-opt) > 1e-6 {
			t.Fatalf("%dx%d: cost %f, optimum %f", n, m, total, opt)
		}
	}
}

func BenchmarkHungarian(b *testing.B) {
	for _, size := range []struct{ n, m int }{{50, 500}, {100, 1000}, {100, 3000}, {100, 5000}, {200, 200}} {
		cm := randomMatrix(rand.New(rand.NewSource(2)), size.n, size.m)
		b.Run(fmt.Sprintf("n%d_m%d", size.n, size.m), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				SolveHungarian(cm)
			}
		})
	}
}
