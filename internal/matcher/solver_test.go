package matcher

import (
	"math/rand"
	"testing"
)

func TestSolverBatchedVsGreedy(t *testing.T) {
	// Property test: The Hungarian assignment should always have a total cost <= the greedy assignment
	for run := 0; run < 1000; run++ {
		N := 10 + rand.Intn(40)
		M := N + rand.Intn(20) // M >= N

		costMatrix := make([][]float64, N)
		for i := 0; i < N; i++ {
			costMatrix[i] = make([]float64, M)
			for j := 0; j < M; j++ {
				costMatrix[i][j] = rand.Float64() * 1000
			}
		}

		greedyAssignment := solveGreedy(costMatrix)
		hungarianAssignment := SolveHungarian(costMatrix)

		greedyCost, _ := calcTotalCost(costMatrix, greedyAssignment)
		hungarianCost, _ := calcTotalCost(costMatrix, hungarianAssignment)

		// Precision error margin
		if hungarianCost > greedyCost+0.0001 {
			t.Errorf("Hungarian cost %f is greater than greedy cost %f", hungarianCost, greedyCost)
		}
	}
}

// Greedy must take the globally cheapest pair first, not walk rows in order.
func TestGreedyTakesGlobalCheapestFirst(t *testing.T) {
	cm := [][]float64{
		{5, 6},
		{1, 100},
	}
	got := solveGreedy(cm)
	if got[0] != 1 || got[1] != 0 {
		t.Fatalf("expected [1 0], got %v", got)
	}
}

func TestCalcTotalCostSkipsInfiniteAndCountsMatches(t *testing.T) {
	cm := [][]float64{
		{3, infiniteCost},
		{infiniteCost, infiniteCost},
	}
	total, matched := calcTotalCost(cm, []int{0, 1})
	if total != 3 || matched != 1 {
		t.Fatalf("got total=%v matched=%v, want 3 and 1", total, matched)
	}
}
