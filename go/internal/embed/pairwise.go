package embed

import "math"

// numpy-compatible float32 summation.
//
// model2vec computes the sentence vector as arr.mean(axis=0) then divides by
// np.linalg.norm — both in float32, and numpy sums with PAIRWISE summation, not
// a running total. Accumulating in float64 (the obvious Go choice) is more
// accurate but produces vectors that differ from numpy's in the last mantissa
// bit, and a 1-ULP difference is enough to reorder near-tied chunks. Since the
// published benchmark numbers were measured against numpy's arithmetic, the
// goal here is to match it, not to improve on it.
//
// The algorithm mirrors numpy's pairwise_sum_FLOAT: naive below 8 elements,
// eight-accumulator unrolling up to 128, and recursive halving above that with
// the split aligned to a multiple of 8.

func pairwiseSum(x []float32) float32 {
	n := len(x)
	switch {
	case n == 0:
		return 0
	case n < 8:
		var res float32
		for _, v := range x {
			res += v
		}
		return res
	case n <= 128:
		// eight independent accumulators, combined in a fixed tree
		var r [8]float32
		for i := 0; i < 8; i++ {
			r[i] = x[i]
		}
		i := 8
		for ; i <= n-8; i += 8 {
			for j := 0; j < 8; j++ {
				r[j] += x[i+j]
			}
		}
		res := ((r[0] + r[1]) + (r[2] + r[3])) + ((r[4] + r[5]) + (r[6] + r[7]))
		for ; i < n; i++ {
			res += x[i]
		}
		return res
	default:
		n2 := n / 2
		n2 -= n2 % 8 // keep the split on the unrolling boundary, as numpy does
		return pairwiseSum(x[:n2]) + pairwiseSum(x[n2:])
	}
}

// meanColumns returns the column-wise mean of rows, matching numpy's
// arr.mean(axis=0) for a float32 array: a pairwise sum per column, divided by
// the row count in float32.
func meanColumns(rows [][]float32, dim int) []float32 {
	out := make([]float32, dim)
	if len(rows) == 0 {
		return out
	}
	col := make([]float32, len(rows))
	for j := 0; j < dim; j++ {
		for i, r := range rows {
			if j < len(r) {
				col[i] = r[j]
			} else {
				col[i] = 0
			}
		}
		out[j] = pairwiseSum(col) / float32(len(rows))
	}
	return out
}

// l2NormalizeF32 reproduces `arr / (np.linalg.norm(arr) + 1e-32)` exactly.
//
// The subtlety is the epsilon. np.linalg.norm returns a float32, but 1e-32 is a
// Python float, so the addition PROMOTES the norm to float64 and the division
// is then float32-array / float64-scalar → float64, rounded to float32 only
// when the vector is packed. Dividing in float32 instead rounds one step
// earlier and lands one ULP low on most components — enough to reorder
// near-tied chunks, which is how this surfaced.
func l2NormalizeF32(v []float32) {
	sq := make([]float32, len(v))
	for i, x := range v {
		sq[i] = x * x
	}
	norm32 := float32(math.Sqrt(float64(pairwiseSum(sq)))) // numpy's float32 norm
	norm64 := float64(norm32) + 1e-32                      // promoted, as in Python
	for i := range v {
		v[i] = float32(float64(v[i]) / norm64)
	}
}
