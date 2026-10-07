//go:build amd64 && !purego

package rerank

import "golang.org/x/sys/cpu"

// The one piece of assembly in the package: the matmul micro-kernel is ~85%
// of a forward pass, and AVX2 FMA does 8 multiply-adds per instruction where
// the Go compiler emits scalar SSE (it does not vectorize, and only fuses
// multiply-add at GOAMD64=v3). Measured, it makes a forward pass several
// times faster. The pure-Go kernel remains the fallback on CPUs without
// AVX2+FMA, on other architectures, and under the purego build tag, and the
// tests check the two agree.

var useFMA = cpu.X86.HasAVX2 && cpu.X86.HasFMA

//go:noescape
func kernel2x4FMA(x0, x1, w0, w1, w2, w3 *float32, n int, out *[8]float32)

func kernel2x4(x0, x1, w0, w1, w2, w3 []float32) [8]float32 {
	n := len(x0)
	if !useFMA || n < 8 {
		return kernel2x4Go(x0, x1, w0, w1, w2, w3)
	}
	x1 = x1[:n]
	w0, w1, w2, w3 = w0[:n], w1[:n], w2[:n], w3[:n]
	var c [8]float32
	n8 := n &^ 7
	kernel2x4FMA(&x0[0], &x1[0], &w0[0], &w1[0], &w2[0], &w3[0], n8, &c)
	if n8 < n {
		t := kernel2x4Go(x0[n8:], x1[n8:], w0[n8:], w1[n8:], w2[n8:], w3[n8:])
		for i := range c {
			c[i] += t[i]
		}
	}
	return c
}
