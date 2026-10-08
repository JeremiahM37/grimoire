package rerank

import (
	"math"
	"math/rand"
	"testing"
)

func randVec(r *rand.Rand, n int) []float32 {
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(r.NormFloat64())
	}
	return v
}

// The dispatching kernel (assembly where available) must agree with the
// pure-Go one, including lengths that are not a multiple of the vector width.
func TestKernelMatchesPureGo(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 7, 8, 9, 31, 32, 384, 389, 1536} {
		v := make([][]float32, 6)
		for i := range v {
			v[i] = randVec(r, n)
		}
		got := kernel2x4(v[0], v[1], v[2], v[3], v[4], v[5])
		want := kernel2x4Go(v[0], v[1], v[2], v[3], v[4], v[5])
		for i := range got {
			if d := math.Abs(float64(got[i] - want[i])); d > 1e-3*math.Sqrt(float64(n)) {
				t.Fatalf("n=%d out[%d]: %v vs %v", n, i, got[i], want[i])
			}
		}
	}
}

// linear must equal a naive y = x·wᵀ + b for shapes with row and column
// tails, at any thread count.
func TestLinearMatchesNaive(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	for _, sh := range []struct{ rows, in, out int }{{1, 8, 4}, {3, 384, 6}, {67, 40, 130}, {33, 1536, 384}} {
		d := &dense{w: randVec(r, sh.out*sh.in), b: randVec(r, sh.out), in: sh.in, out: sh.out}
		x := randVec(r, sh.rows*sh.in)
		for _, threads := range []int{1, 7} {
			y := make([]float32, sh.rows*sh.out)
			linear(threads, y, x, sh.rows, d, actNone)
			for i := 0; i < sh.rows; i++ {
				for j := 0; j < sh.out; j++ {
					var want float64
					for k := 0; k < sh.in; k++ {
						want += float64(x[i*sh.in+k]) * float64(d.w[j*sh.in+k])
					}
					want += float64(d.b[j])
					if diff := math.Abs(want - float64(y[i*sh.out+j])); diff > 1e-3*math.Sqrt(float64(sh.in)) {
						t.Fatalf("%v threads=%d y[%d,%d]=%v want %v", sh, threads, i, j, y[i*sh.out+j], want)
					}
				}
			}
		}
	}
}

func BenchmarkKernel(b *testing.B) {
	r := rand.New(rand.NewSource(3))
	v := make([][]float32, 6)
	for i := range v {
		v[i] = randVec(r, 384)
	}
	for _, k := range []struct {
		name string
		f    func(a, b, c, d, e, f []float32) [8]float32
	}{{"dispatch", kernel2x4}, {"purego", kernel2x4Go}} {
		b.Run(k.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				k.f(v[0], v[1], v[2], v[3], v[4], v[5])
			}
			b.ReportMetric(float64(b.N)*8*384*2/b.Elapsed().Seconds()/1e9, "GFLOP/s")
		})
	}
}

func TestExpAndErfApproximations(t *testing.T) {
	var worstExp, worstErf float64
	for x := -87.0; x <= 88.5; x += 0.0137 {
		got, want := float64(exp32(float32(x))), math.Exp(float64(float32(x)))
		worstExp = math.Max(worstExp, math.Abs(got-want)/want)
	}
	for x := -6.0; x <= 6; x += 0.001 {
		got, want := float64(erf32(float32(x))), math.Erf(float64(float32(x)))
		worstErf = math.Max(worstErf, math.Abs(got-want))
	}
	if worstExp > 1e-6 {
		t.Errorf("exp32 relative error %.3g", worstExp)
	}
	if worstErf > 5e-7 {
		t.Errorf("erf32 absolute error %.3g", worstErf)
	}
	if exp32(-100) != 0 || !math.IsInf(float64(exp32(100)), 1) {
		t.Error("exp32 range clamps")
	}
	t.Logf("exp32 max rel err %.3g, erf32 max abs err %.3g", worstExp, worstErf)
}
