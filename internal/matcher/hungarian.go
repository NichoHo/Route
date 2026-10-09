package matcher

import "math"

// SolveHungarian solves the linear assignment problem with the Hungarian
// algorithm. costMatrix[i][j] is the cost of assigning row i to column j.
// It returns a slice of length len(costMatrix) where result[i] is the column
// assigned to row i, or -1 if row i is left unassigned (more rows than
// columns). Costs must be non-negative.
//
// The matrix is solved as-is, without padding it to a square: the work is
// O(n^2 * m) with n = min(rows, cols) and m = max(rows, cols). A tall matrix
// is transposed first so the algorithm always has n <= m.
func SolveHungarian(costMatrix [][]float64) []int {
	nRows := len(costMatrix)
	if nRows == 0 {
		return nil
	}
	nCols := len(costMatrix[0])

	assignment := make([]int, nRows)
	for i := range assignment {
		assignment[i] = -1
	}
	if nCols == 0 {
		return assignment
	}

	if nRows <= nCols {
		rowToCol := hungarianWide(costMatrix, nRows, nCols)
		copy(assignment, rowToCol)
		return assignment
	}

	// More rows than columns: solve the transpose, then invert the result.
	t := make([][]float64, nCols)
	for j := range t {
		t[j] = make([]float64, nRows)
		for i := 0; i < nRows; i++ {
			t[j][i] = costMatrix[i][j]
		}
	}
	colToRow := hungarianWide(t, nCols, nRows)
	for col, row := range colToRow {
		assignment[row] = col
	}
	return assignment
}

// hungarianWide assigns every one of the n rows to a distinct column of the
// m >= n columns, minimising total cost. It returns the column for each row.
func hungarianWide(a [][]float64, n, m int) []int {
	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1) // p[j] = row (1-based) matched to column j, 0 if free
	way := make([]int, m+1)
	minv := make([]float64, m+1)
	used := make([]bool, m+1)

	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		for j := 0; j <= m; j++ {
			minv[j] = math.MaxFloat64
			used[j] = false
		}

		for {
			used[j0] = true
			i0 := p[j0]
			delta := math.MaxFloat64
			j1 := 0
			row := a[i0-1]
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := row[j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}

		for j0 != 0 {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
		}
	}

	rowToCol := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			rowToCol[p[j]-1] = j - 1
		}
	}
	return rowToCol
}

// HungarianWork estimates the operation count of SolveHungarian for an
// nRows x nCols matrix: O(min^2 * max).
func HungarianWork(nRows, nCols int) int {
	lo, hi := nRows, nCols
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo * lo * hi
}
