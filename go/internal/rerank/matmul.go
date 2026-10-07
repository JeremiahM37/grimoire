package rerank

import (
	"math"
	"sync"
	"sync/atomic"
)

// The forward pass is almost entirely y = x·wᵀ + b over a few thousand token
// rows, so this file is where the time goes.
//
// Layout: x is [rows × in] and w is [out × in], both row-major, so each output
// is a dot product of two contiguous vectors. The work is cut into tiles of
// tileRows rows × colsPerTile(in) outputs, sized so the tile's weight rows
// (~192 KB) stay resident in a core's L2 while every row of the tile streams
// past them; tiles are handed to workers from an atomic counter.
//
// Inside a tile the micro-kernel computes a 2-row × 4-column block per pass
// over the input dimension: 8 independent accumulators, 6 loads per 8
// multiply-adds. Measured on amd64 it is about twice as fast as 1×8, 4×2 or
// 4×4 blocks, which run out of the 16 SSE registers the Go compiler
// allocates and spill accumulators to the stack.

const (
	tileRows       = 32
	tileWeightBuff = 48 << 10 // float32s of weights per tile (192 KB)
)

type activation int

const (
	actNone activation = iota
	actGELU
	actTanh
)

func colsPerTile(in int) int {
	n := tileWeightBuff / in
	n -= n % 4
	return max(4, n)
}

// linear computes y[r] = act(x[r]·wᵀ + b) for rows r of x.
func linear(threads int, y, x []float32, rows int, d *dense, act activation) {
	in, out := d.in, d.out
	x, y = x[:rows*in], y[:rows*out]
	nc := colsPerTile(in)
	rowTiles := (rows + tileRows - 1) / tileRows
	colTiles := (out + nc - 1) / nc
	parallelTasks(threads, rowTiles*colTiles, func(_, task int) {
		// column-major task order: consecutive tasks share weight columns,
		// which a neighbouring core may still have in the shared L3
		r0 := (task % rowTiles) * tileRows
		c0 := (task / rowTiles) * nc
		linearTile(y, x, d, r0, min(r0+tileRows, rows), c0, min(c0+nc, out), act)
	})
}

func linearTile(y, x []float32, d *dense, r0, r1, c0, c1 int, act activation) {
	in, out := d.in, d.out
	gemmT(y[r0*out+c0:], out, x[r0*in:], in, d.w[c0*in:], in, r1-r0, c1-c0, in)
	bias := d.b[c0:c1]
	for i := r0; i < r1; i++ {
		row := y[i*out+c0 : i*out+c1]
		switch act {
		case actNone:
			add(row, bias)
		case actGELU:
			for k := range row {
				row[k] = gelu(row[k] + bias[k])
			}
		case actTanh:
			for k := range row {
				row[k] = float32(math.Tanh(float64(row[k] + bias[k])))
			}
		}
	}
}

// gemmT sets y[i*ldy+j] = Σ_k x[i*ldx+k]·w[j*ldw+k] for i < rows, j < cols,
// k < n: a product against a transposed operand, so both inputs are read
// along contiguous rows. Serial; callers parallelize over tiles.
func gemmT(y []float32, ldy int, x []float32, ldx int, w []float32, ldw int, rows, cols, n int) {
	j := 0
	for ; j+4 <= cols; j += 4 {
		w0 := w[j*ldw : j*ldw+n]
		w1 := w[(j+1)*ldw : (j+1)*ldw+n]
		w2 := w[(j+2)*ldw : (j+2)*ldw+n]
		w3 := w[(j+3)*ldw : (j+3)*ldw+n]
		i := 0
		for ; i+2 <= rows; i += 2 {
			c := kernel2x4(x[i*ldx:i*ldx+n], x[(i+1)*ldx:(i+1)*ldx+n], w0, w1, w2, w3)
			y0 := y[i*ldy+j : i*ldy+j+4]
			y1 := y[(i+1)*ldy+j : (i+1)*ldy+j+4]
			y0[0], y0[1], y0[2], y0[3] = c[0], c[1], c[2], c[3]
			y1[0], y1[1], y1[2], y1[3] = c[4], c[5], c[6], c[7]
		}
		if i < rows {
			// a lone row still goes through the vector kernel, paired with
			// itself: half the work is wasted, and it is still faster
			xi := x[i*ldx : i*ldx+n]
			c := kernel2x4(xi, xi, w0, w1, w2, w3)
			y0 := y[i*ldy+j : i*ldy+j+4]
			y0[0], y0[1], y0[2], y0[3] = c[0], c[1], c[2], c[3]
		}
	}
	for ; j < cols; j++ {
		wj := w[j*ldw : j*ldw+n]
		for i := 0; i < rows; i++ {
			y[i*ldy+j] = dot(x[i*ldx:i*ldx+n], wj)
		}
	}
}

// kernel2x4Go returns the 2×4 block of dot products between rows x0,x1 and
// rows w0..w3: c[0:4] for x0, c[4:8] for x1. The reslicing to len(x0) lets
// the compiler drop every bounds check in the loop.
func kernel2x4Go(x0, x1, w0, w1, w2, w3 []float32) [8]float32 {
	n := len(x0)
	x1 = x1[:n]
	w0, w1, w2, w3 = w0[:n], w1[:n], w2[:n], w3[:n]
	var c00, c01, c02, c03, c10, c11, c12, c13 float32
	for k := 0; k < n; k++ {
		a0, a1 := x0[k], x1[k]
		b0, b1, b2, b3 := w0[k], w1[k], w2[k], w3[k]
		c00 += a0 * b0
		c01 += a0 * b1
		c02 += a0 * b2
		c03 += a0 * b3
		c10 += a1 * b0
		c11 += a1 * b1
		c12 += a1 * b2
		c13 += a1 * b3
	}
	return [8]float32{c00, c01, c02, c03, c10, c11, c12, c13}
}

// gelu is the exact (erf) GELU BERT was trained with — not the tanh
// approximation, which moves the logits by more than the parity tolerance.
func gelu(x float32) float32 {
	return 0.5 * x * (1 + erf32(x*(1/math.Sqrt2)))
}

// erf32 is erf to within 1.5e-7 absolute (Abramowitz & Stegun 7.1.26), the
// same approximation vectorized CPU inference kernels use. math.Erf is exact
// in float64 and was 13% of a forward pass.
func erf32(x float32) float32 {
	sign := float32(1)
	if x < 0 {
		sign, x = -1, -x
	}
	t := 1 / (1 + 0.3275911*x)
	p := t * (0.254829592 + t*(-0.284496736+t*(1.421413741+t*(-1.453152027+t*1.061405429))))
	return sign * (1 - p*exp32(-x*x))
}

// exp32 is e^x in float32 to about 2 ulp: Cody–Waite reduction to
// x = k·ln2 + r with |r| ≤ ln2/2, a degree-7 Taylor polynomial for e^r, and
// 2^k assembled in the exponent bits. Results below the normal range flush
// to zero, which only ever happens to softmax terms that cannot matter.
// Inputs are clamped so k stays in [-126, 128].
func exp32(x float32) float32 {
	const (
		log2e = 1.44269504088896341
		ln2hi = 0.693145751953125
		ln2lo = 1.428606765330187045e-06
	)
	if x < -87.33 {
		return 0
	}
	if x > 88.72 {
		return float32(math.Inf(1))
	}
	kf := x * log2e
	if kf < 0 {
		kf -= 0.5
	} else {
		kf += 0.5
	}
	k := int32(kf)
	fk := float32(k)
	r := x - fk*ln2hi - fk*ln2lo
	p := 1 + r*(1+r*(1.0/2+r*(1.0/6+r*(1.0/24+r*(1.0/120+r*(1.0/720+r*(1.0/5040)))))))
	if k > 127 { // 2^128 is not a float32; e^x still is, just below overflow
		return p * math.Float32frombits(uint32(127+127)<<23) * 2
	}
	return p * math.Float32frombits(uint32(k+127)<<23)
}

// parallelTasks runs fn for task 0..n-1 on up to threads goroutines. worker
// is a stable index in [0, threads) for per-worker scratch.
func parallelTasks(threads, n int, fn func(worker, task int)) {
	threads = min(threads, n)
	if threads <= 1 {
		for t := 0; t < n; t++ {
			fn(0, t)
		}
		return
	}
	var next atomic.Int64
	var wg sync.WaitGroup
	wg.Add(threads)
	for w := 0; w < threads; w++ {
		go func(w int) {
			defer wg.Done()
			for {
				t := int(next.Add(1) - 1)
				if t >= n {
					return
				}
				fn(w, t)
			}
		}(w)
	}
	wg.Wait()
}

// parallelRows splits rows 0..n-1 into chunks of at least minChunk.
func parallelRows(threads, n, minChunk int, fn func(lo, hi int)) {
	chunks := max(1, min(threads, (n+minChunk-1)/minChunk))
	size := (n + chunks - 1) / chunks
	parallelTasks(threads, chunks, func(_, c int) {
		lo := c * size
		fn(lo, min(lo+size, n))
	})
}
