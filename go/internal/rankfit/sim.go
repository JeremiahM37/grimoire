package rankfit

import (
	"math"
	"math/rand/v2"
)

// The simulator plants a relevance model and a position-bias model, then logs
// data the way the live trace would. Because the truth is known, it can test
// whether the learner and the off-policy estimator recover it. It is evidence
// about the estimators under the stated assumptions, not about real use.

// LoggingWeights are the hand-set ranking weights the logger ranks by. They
// mirror the live rankMemory constants on the five scored components; link and
// authority carry no weight today.
var LoggingWeights = []float64{0.42, 0.28, 0.18, 0.04, 0.08, 0, 0}

// TruthWeights are the planted relevance weights. They sum to 1, so relevance
// is in [0,1] for features in [0,1]. They differ from LoggingWeights mainly in
// recency, which the logger under-weights.
var TruthWeights = []float64{0.05, 0.10, 0.10, 0.45, 0.20, 0.05, 0.05}

// SimConfig describes the logging process.
type SimConfig struct {
	K       int       // slate size shown to the agent (default 8)
	Pool    int       // candidates drawn per slate; the logger keeps the top K (default 20)
	Explore float64   // share of slates whose order is shuffled (default 0.2)
	Holdout float64   // probability each eligible item is withheld (default 0.2)
	Alpha   float64   // examination exponent: exam(k) = k^-Alpha (default 1)
	Logging []float64 // logging policy weights (default LoggingWeights)
	Truth   []float64 // planted relevance weights (default TruthWeights)
}

func (c SimConfig) withDefaults() SimConfig {
	if c.K == 0 {
		c.K = 8
	}
	if c.Pool == 0 {
		c.Pool = 20
	}
	if c.Explore == 0 {
		c.Explore = 0.2
	}
	if c.Holdout == 0 {
		c.Holdout = 0.2
	}
	if c.Alpha == 0 {
		c.Alpha = 1
	}
	if c.Logging == nil {
		c.Logging = LoggingWeights
	}
	if c.Truth == nil {
		c.Truth = TruthWeights
	}
	return c
}

// TrueExam returns the planted examination curve for K positions.
func (c SimConfig) TrueExam() []float64 {
	c = c.withDefaults()
	out := make([]float64, c.K)
	for k := range out {
		out[k] = math.Pow(float64(k+1), -c.Alpha)
	}
	return out
}

// Sim is a simulated log with its ground truth.
type Sim struct {
	Slates [][]Item
	Rel    [][]float64 // true relevance of each item, parallel to Slates
}

// Simulate draws n slates. The logger draws a pool, keeps the top K by its own
// weights, shuffles the order on an explored slate, and withholds each item
// with probability Holdout. A shown item's use is Bernoulli(exam(pos) x
// relevance); a withheld item has no outcome.
func Simulate(cfg SimConfig, n int, seed uint64) Sim {
	cfg = cfg.withDefaults()
	rng := rand.New(rand.NewPCG(seed, 0xa0761d6478bd642f))
	out := Sim{Slates: make([][]Item, 0, n), Rel: make([][]float64, 0, n)}
	exam := cfg.TrueExam()
	for s := 0; s < n; s++ {
		pool := make([][]float64, cfg.Pool)
		for i := range pool {
			pool[i] = make([]float64, Dims)
			for d := range pool[i] {
				pool[i][d] = rng.Float64()
			}
		}
		ranked := Ranks(poolItems(pool), cfg.Logging)
		top := make([][]float64, cfg.K)
		for i, r := range ranked {
			if r <= cfg.K {
				top[r-1] = pool[i]
			}
		}
		explored := rng.Float64() < cfg.Explore
		order := rng.Perm(cfg.K)
		if !explored {
			order = identity(cfg.K)
		}
		slate := make([]Item, cfg.K)
		rel := make([]float64, cfg.K)
		id := slateID(s)
		for pos, src := range order {
			f := top[src]
			r := dot(cfg.Truth, f)
			shown := rng.Float64() >= cfg.Holdout
			it := Item{Slate: id, Pos: pos + 1, Explored: explored, Shown: shown,
				PShow: 1 - cfg.Holdout, Feat: append([]float64(nil), f...)}
			if shown && rng.Float64() < exam[pos]*r {
				it.Used = 1
			}
			slate[pos] = it
			rel[pos] = r
		}
		out.Slates = append(out.Slates, slate)
		out.Rel = append(out.Rel, rel)
	}
	return out
}

// Truth draws n fresh slates and returns the expected value of policy w on them
// under the planted model. The logged-outcome fields are ignored.
func Truth(cfg SimConfig, n int, seed uint64, w []float64) float64 {
	cfg = cfg.withDefaults()
	sim := Simulate(cfg, n, seed)
	return TrueValue(sim.Slates, sim.Rel, cfg.TrueExam(), w)
}

func poolItems(pool [][]float64) []Item {
	items := make([]Item, len(pool))
	for i, f := range pool {
		items[i] = Item{Feat: f}
	}
	return items
}

func identity(n int) []int {
	out := make([]int, n)
	for i := range out {
		out[i] = i
	}
	return out
}

func slateID(s int) string {
	const digits = "0123456789abcdef"
	b := make([]byte, 8)
	for i := range b {
		b[i] = digits[(s>>(4*i))&15]
	}
	return string(b)
}
