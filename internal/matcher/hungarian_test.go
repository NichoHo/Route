package matcher_test

import (
	"reflect"
	"testing"

	"route/internal/matcher"
)

func TestSolveHungarian(t *testing.T) {
	// A simple 3x3 case
	costMatrix := [][]float64{
		{2, 1, 3},
		{4, 3, 2},
		{1, 2, 4},
	}

	ans := matcher.SolveHungarian(costMatrix)
	// row 0 should pick col 1 (cost 1)
	// row 1 should pick col 2 (cost 2)
	// row 2 should pick col 0 (cost 1)
	// total = 4
	expected := []int{1, 2, 0}

	if !reflect.DeepEqual(ans, expected) {
		t.Errorf("expected %v, got %v", expected, ans)
	}

	// Rectangular matrix (N > M) - More rows than columns
	costMatrix2 := [][]float64{
		{1, 10},
		{10, 1},
		{2, 2},
	}
	ans2 := matcher.SolveHungarian(costMatrix2)
	// 0 -> 0, 1 -> 1, 2 -> -1
	// Actually, Hungarian pads to 3x3.
	// row 0 (1, 10, pad), row 1 (10, 1, pad), row 2 (2, 2, pad). pad=11
	// It's optimal to give row 2 the pad, meaning unassigned.
	expected2 := []int{0, 1, -1}
	if !reflect.DeepEqual(ans2, expected2) {
		t.Errorf("expected %v, got %v", expected2, ans2)
	}
}
