package estimate

import (
	"math"
	"math/rand/v2"
	"testing"
)

// simulate draws records from a world with a known effect. Baseline success
// depends on a stratum (a confounder for the outcome, not for exposure, which
// is randomized). Outcomes are Bernoulli.
func simulate(rng *rand.Rand, n int, effect, propensity float64) []Record {
	recs := make([]Record, n)
	for i := range recs {
		s := rng.IntN(3)
		base := []float64{0.2, 0.5, 0.7}[s]
		exposed := rng.Float64() < propensity
		p := base
		if exposed {
			p = base + effect
		}
		y := 0.0
		if rng.Float64() < p {
			y = 1
		}
		recs[i] = Record{Exposed: exposed, Propensity: propensity, Outcome: y, Stratum: string(rune('a' + s))}
	}
	return recs
}

func TestCoverageAnalytic(t *testing.T) {
	for _, m := range []Method{IPS, DR} {
		rng := rand.New(rand.NewPCG(7, uint64(m)+1))
		const reps, n, truth = 1000, 400, 0.10
		cover, sum := 0, 0.0
		for r := 0; r < reps; r++ {
			est, err := Lift(simulate(rng, n, truth, 0.8), Options{Method: m})
			if err != nil || est.Status != OK {
				t.Fatal(err, est)
			}
			sum += est.Lift
			if est.Lo <= truth && truth <= est.Hi {
				cover++
			}
		}
		c := float64(cover) / reps
		bias := sum/reps - truth
		t.Logf("%s: coverage %.3f bias %+.4f", m, c, bias)
		if c < 0.92 || c > 0.98 {
			t.Errorf("%s coverage %.3f outside [0.92,0.98]", m, c)
		}
		if math.Abs(bias) > 0.01 {
			t.Errorf("%s biased: %+.4f", m, bias)
		}
	}
}

func TestDRLowerVarianceThanIPS(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 4))
	var ips, dr []float64
	for r := 0; r < 300; r++ {
		recs := simulate(rng, 300, 0.1, 0.8)
		a, _ := Lift(recs, Options{Method: IPS})
		b, _ := Lift(recs, Options{Method: DR})
		ips = append(ips, a.Lift)
		dr = append(dr, b.Lift)
	}
	_, vi := meanVar(ips)
	_, vd := meanVar(dr)
	t.Logf("var IPS %.5f DR %.5f", vi, vd)
	if vd >= vi {
		t.Errorf("DR variance %.5f not below IPS %.5f", vd, vi)
	}
}

func TestCoverageBootstrap(t *testing.T) {
	rng := rand.New(rand.NewPCG(11, 12))
	const reps, n, truth = 200, 300, -0.08
	cover := 0
	for r := 0; r < reps; r++ {
		est, err := LiftBootstrap(simulate(rng, n, truth, 0.7), Options{Method: DR}, 300, uint64(r+1))
		if err != nil || est.Status != OK {
			t.Fatal(err, est)
		}
		if est.Lo <= truth && truth <= est.Hi {
			cover++
		}
	}
	c := float64(cover) / reps
	t.Logf("bootstrap DR coverage %.3f", c)
	if c < 0.90 || c > 0.99 {
		t.Errorf("bootstrap coverage %.3f", c)
	}
}

func TestVaryingPropensityUnbiased(t *testing.T) {
	// propensities differ per record (for example higher for rules); IPS stays unbiased.
	rng := rand.New(rand.NewPCG(5, 6))
	const truth = 0.15
	sum := 0.0
	const reps = 400
	for r := 0; r < reps; r++ {
		recs := make([]Record, 300)
		for i := range recs {
			e := []float64{0.5, 0.9, 0.95}[rng.IntN(3)]
			exp := rng.Float64() < e
			p := 0.3
			if exp {
				p += truth
			}
			y := 0.0
			if rng.Float64() < p {
				y = 1
			}
			recs[i] = Record{Exposed: exp, Propensity: e, Outcome: y}
		}
		est, _ := Lift(recs, Options{Method: IPS})
		sum += est.Lift
	}
	if b := sum/reps - truth; math.Abs(b) > 0.015 {
		t.Errorf("bias %+.4f", b)
	}
}

func TestNullEffectRarelySignificant(t *testing.T) {
	rng := rand.New(rand.NewPCG(8, 9))
	fp := 0
	const reps = 500
	for r := 0; r < reps; r++ {
		est, _ := Lift(simulate(rng, 300, 0, 0.8), Options{Method: DR})
		if est.Lo > 0 || est.Hi < 0 {
			fp++
		}
	}
	if rate := float64(fp) / reps; rate > 0.08 {
		t.Errorf("false-positive rate %.3f", rate)
	}
}

func TestInsufficientGuards(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 1))
	recs := simulate(rng, 200, 0.1, 0.95)
	// only a handful withheld
	var few []Record
	w := 0
	for _, r := range recs {
		if !r.Exposed {
			if w >= 4 {
				continue
			}
			w++
		}
		few = append(few, r)
	}
	for _, m := range []Method{IPS, DR} {
		est, err := Lift(few, Options{Method: m})
		if err != nil || est.Status != Insufficient || est.Reason == "" {
			t.Errorf("%s: want insufficient, got %+v err %v", m, est, err)
		}
		b, _ := LiftBootstrap(few, Options{Method: m}, 100, 1)
		if b.Status != Insufficient {
			t.Errorf("bootstrap should be insufficient")
		}
	}
	if est, _ := Lift(nil, Options{}); est.Status != Insufficient {
		t.Error("empty must be insufficient")
	}
	if est, _ := Lift(recs, Options{MinExposed: 10000}); est.Status != Insufficient {
		t.Error("custom guard ignored")
	}
}

func TestBadRecords(t *testing.T) {
	for _, p := range []float64{0, 1, -1, 2, math.NaN()} {
		if _, err := Lift([]Record{{Exposed: true, Propensity: p, Outcome: 1}}, Options{}); err == nil {
			t.Errorf("propensity %v accepted", p)
		}
	}
	if _, err := Lift([]Record{{Propensity: .5, Outcome: math.NaN()}}, Options{}); err == nil {
		t.Error("NaN outcome accepted")
	}
}

func TestZQuantile(t *testing.T) {
	if z := zQuantile(0.975); math.Abs(z-1.959964) > 1e-5 {
		t.Errorf("z=%v", z)
	}
	if z := zQuantile(0.5); math.Abs(z) > 1e-9 {
		t.Errorf("z=%v", z)
	}
}

func TestDeterministic(t *testing.T) {
	recs := simulate(rand.New(rand.NewPCG(2, 2)), 200, 0.1, 0.8)
	a, _ := Lift(recs, Options{Method: DR})
	b, _ := Lift(recs, Options{Method: DR})
	if a != b {
		t.Error("DR not deterministic")
	}
}

func TestPoolShrinkageReducesError(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	var rawSE, shrSE float64
	var rawCov, shrCov, tot int
	for rep := 0; rep < 30; rep++ {
		var mems []MemoryData
		var truth []float64
		for i := 0; i < 20; i++ {
			th := 0.10 + 0.03*rng.NormFloat64() // kind mean 0.10, tau 0.03
			truth = append(truth, th)
			mems = append(mems, MemoryData{ID: "m", Kind: "rule", Records: simulate(rng, 80, th, 0.8)})
		}
		est, kinds, err := Pool(mems, PoolOptions{Lift: Options{Method: DR}})
		if err != nil {
			t.Fatal(err)
		}
		if kinds[0].Status != OK {
			t.Fatalf("kind: %+v", kinds[0])
		}
		for i, e := range est {
			if e.Raw.Status != OK || e.Shrunk.Status != OK {
				continue
			}
			tot++
			rawSE += (e.Raw.Lift - truth[i]) * (e.Raw.Lift - truth[i])
			shrSE += (e.Shrunk.Lift - truth[i]) * (e.Shrunk.Lift - truth[i])
			if e.Raw.Lo <= truth[i] && truth[i] <= e.Raw.Hi {
				rawCov++
			}
			if e.Shrunk.Lo <= truth[i] && truth[i] <= e.Shrunk.Hi {
				shrCov++
			}
			if e.Weight < 0 || e.Weight > 1 {
				t.Fatalf("weight %v", e.Weight)
			}
		}
	}
	t.Logf("MSE raw %.5f shrunk %.5f; coverage raw %.3f shrunk %.3f", rawSE/float64(tot), shrSE/float64(tot), float64(rawCov)/float64(tot), float64(shrCov)/float64(tot))
	if shrSE >= rawSE {
		t.Error("shrinkage did not reduce squared error")
	}
	if c := float64(shrCov) / float64(tot); c < 0.90 {
		t.Errorf("shrunk coverage %.3f", c)
	}
}

func TestPoolInsufficientAndSmallKinds(t *testing.T) {
	rng := rand.New(rand.NewPCG(31, 32))
	mems := []MemoryData{
		{ID: "a", Kind: "rule", Records: simulate(rng, 200, 0.1, 0.8)},
		{ID: "b", Kind: "rule", Records: simulate(rng, 200, 0.1, 0.8)},
		{ID: "c", Kind: "rule", Records: simulate(rng, 200, 0.1, 0.8)},
		{ID: "d", Kind: "rule", Records: simulate(rng, 5, 0.1, 0.8)},   // too little
		{ID: "e", Kind: "fact", Records: simulate(rng, 200, 0.1, 0.8)}, // kind of one
	}
	est, kinds, err := Pool(mems, PoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if est[3].Raw.Status != Insufficient || est[3].Shrunk.Status != Insufficient {
		t.Errorf("d: %+v", est[3])
	}
	if est[4].Shrunk != est[4].Raw || est[4].Weight != 1 {
		t.Errorf("small kind must keep raw: %+v", est[4])
	}
	got := map[string]Status{}
	for _, k := range kinds {
		got[k.Kind] = k.Status
	}
	if got["rule"] != OK || got["fact"] != Insufficient {
		t.Errorf("kinds %v", got)
	}
}

func TestRandomEffectsHomogeneous(t *testing.T) {
	mu, se, tau2 := randomEffects([]float64{0.1, 0.1, 0.1}, []float64{0.01, 0.01, 0.01})
	if math.Abs(mu-0.1) > 1e-12 || tau2 != 0 || math.Abs(se-math.Sqrt(0.01/3)) > 1e-12 {
		t.Errorf("%v %v %v", mu, se, tau2)
	}
}

func TestUtilityOnlyOnExposure(t *testing.T) {
	u := NewUtility(0.5, 0.2, 3)
	if u.Observe(false, 1) || u.N != 0 || u.Q != 0.5 {
		t.Fatal("withheld must not update")
	}
	if _, ok := u.Value(); ok {
		t.Fatal("no data must not be ok")
	}
	u.Observe(true, 1)
	if want := 0.5 + 0.2*(1-0.5); math.Abs(u.Q-want) > 1e-12 {
		t.Errorf("Q=%v want %v", u.Q, want)
	}
	u.Observe(false, 0)
	u.Observe(true, 1)
	if u.N != 2 {
		t.Errorf("N=%d", u.N)
	}
	if _, ok := u.Value(); ok {
		t.Error("n below MinN must not be ok")
	}
	u.Observe(true, 1)
	q, ok := u.Value()
	if !ok || q <= 0.5 {
		t.Errorf("q=%v ok=%v", q, ok)
	}
	u.Observe(true, math.NaN())
	if u.N != 3 {
		t.Error("NaN reward counted")
	}
}

func TestUtilityRunningMeanAndConvergence(t *testing.T) {
	u := NewUtility(0.9, 0, 1)
	for _, r := range []float64{1, 0, 1, 1} {
		u.Observe(true, r)
	}
	if math.Abs(u.Q-0.75) > 1e-12 {
		t.Errorf("running mean %v", u.Q)
	}
	rng := rand.New(rand.NewPCG(1, 9))
	v := NewUtility(0, 0.05, 1)
	for i := 0; i < 5000; i++ {
		r := 0.0
		if rng.Float64() < 0.7 {
			r = 1
		}
		v.Observe(true, r)
	}
	if math.Abs(v.Q-0.7) > 0.12 {
		t.Errorf("EMA %v far from 0.7", v.Q)
	}
}
