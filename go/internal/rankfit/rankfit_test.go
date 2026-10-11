package rankfit

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// seeds is the number of independent simulated logs each claim is checked on.
const seeds = 20

// studySlates is the log size claims (b) and (c) are checked at. At 4000
// slates the propensity learner misses Tolerance on most seeds (see
// TestSampleSizeCurve), so the claims are stated at 16000.
const studySlates = 16000

type seedResult struct {
	Seed         uint64  `json:"seed"`
	Slates       int     `json:"slates"`
	Exposures    int     `json:"exposures"`
	L1Naive      float64 `json:"l1_naive"`
	L1IPS        float64 `json:"l1_ips"`
	L1Logging    float64 `json:"l1_logging"`
	CosNaive     float64 `json:"cos_naive"`
	CosIPS       float64 `json:"cos_ips"`
	TrueCur      float64 `json:"true_current"`
	TrueLearned  float64 `json:"true_learned"`
	IPSCur       float64 `json:"ips_current"`
	IPSLearned   float64 `json:"ips_learned"`
	DRCur        float64 `json:"dr_current"`
	DRLearned    float64 `json:"dr_learned"`
	SignIPS      bool    `json:"sign_ips_agrees"`
	SignDR       bool    `json:"sign_dr_agrees"`
	ExamErrMax   float64 `json:"exam_err_max"`
	NaiveTowards string  `json:"naive_closer_to"`
}

// run fits and evaluates one simulated log.
func run(cfg SimConfig, nTrain int, seed uint64, truthSeed uint64) (seedResult, error) {
	cfg = cfg.withDefaults()
	train := Simulate(cfg, nTrain, seed)
	test := Simulate(cfg, nTrain, seed+7919)
	var items []Item
	for _, s := range train.Slates {
		items = append(items, s...)
	}
	exam, err := EstimateExam(items, cfg.K, Options{})
	if err != nil {
		return seedResult{}, err
	}
	learned, err := FitPropensity(items, exam.Curve, Options{})
	if err != nil {
		return seedResult{}, err
	}
	naive, err := FitNaive(items, Options{})
	if err != nil {
		return seedResult{}, err
	}
	truthExam := cfg.TrueExam()
	res := seedResult{Seed: seed, Slates: nTrain, Exposures: len(items)}
	res.L1IPS = L1(learned, cfg.Truth)
	res.L1Naive = L1(naive, cfg.Truth)
	res.L1Logging = L1(cfg.Logging, cfg.Truth)
	res.CosIPS = Cosine(learned, cfg.Truth)
	res.CosNaive = Cosine(naive, cfg.Truth)
	res.NaiveTowards = "truth"
	if L1(naive, cfg.Logging) < L1(naive, cfg.Truth) {
		res.NaiveTowards = "logging"
	}
	for k := range truthExam {
		if d := math.Abs(exam.Curve[k] - truthExam[k]); d > res.ExamErrMax {
			res.ExamErrMax = d
		}
	}
	res.TrueCur = Truth(cfg, 20000, truthSeed, cfg.Logging)
	res.TrueLearned = Truth(cfg, 20000, truthSeed, learned)
	res.IPSCur = EvaluateIPS(test.Slates, exam.Curve, cfg.Logging)
	res.IPSLearned = EvaluateIPS(test.Slates, exam.Curve, learned)
	res.DRCur, err = EvaluateDR(test.Slates, exam.Curve, cfg.Logging, Options{})
	if err != nil {
		return res, err
	}
	res.DRLearned, err = EvaluateDR(test.Slates, exam.Curve, learned, Options{})
	if err != nil {
		return res, err
	}
	res.SignIPS = (res.IPSLearned > res.IPSCur) == (res.TrueLearned > res.TrueCur)
	res.SignDR = (res.DRLearned > res.DRCur) == (res.TrueLearned > res.TrueCur)
	return res, nil
}

// TestExamIsRecoveredFromExploration checks the position curve against the
// planted one.
func TestExamIsRecoveredFromExploration(t *testing.T) {
	cfg := SimConfig{}
	sim := Simulate(cfg, 4000, 1)
	var items []Item
	for _, s := range sim.Slates {
		items = append(items, s...)
	}
	exam, err := EstimateExam(items, 8, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := cfg.TrueExam()
	for k := range want {
		if math.Abs(exam.Curve[k]-want[k]) > 0.05 {
			t.Fatalf("position %d: estimated %.3f, planted %.3f", k+1, exam.Curve[k], want[k])
		}
	}
}

// TestExamNeedsExploration checks that without explored slates the learner
// refuses to correct for position rather than guessing.
func TestExamNeedsExploration(t *testing.T) {
	sim := Simulate(SimConfig{Explore: 1e-9}, 2000, 2)
	var items []Item
	for _, s := range sim.Slates {
		items = append(items, s...)
	}
	if _, err := EstimateExam(items, 8, Options{}); !errors.Is(err, ErrNoExploration) {
		t.Fatalf("want ErrNoExploration, got %v", err)
	}
}

// TestNaiveLearnsTheLoggingPolicy is claim (a): regressing use on features
// with no position correction returns the logger's weights, not the planted
// ones.
func TestNaiveLearnsTheLoggingPolicy(t *testing.T) {
	cfg := SimConfig{}
	for s := uint64(0); s < seeds; s++ {
		res, err := run(cfg, 4000, 100+s, 500+s)
		if err != nil {
			t.Fatal(err)
		}
		if res.NaiveTowards != "logging" {
			t.Errorf("seed %d: naive fit is closer to the planted weights (L1 %.3f) than to the logging weights",
				s, res.L1Naive)
		}
		if res.L1Naive <= res.L1IPS {
			t.Errorf("seed %d: naive L1 %.3f not worse than propensity-weighted %.3f", s, res.L1Naive, res.L1IPS)
		}
	}
}

// TestPropensityLearnerRecoversPlantedWeights is claim (b).
func TestPropensityLearnerRecoversPlantedWeights(t *testing.T) {
	cfg := SimConfig{}
	fails := 0
	for s := uint64(0); s < seeds; s++ {
		res, err := run(cfg, studySlates, 100+s, 500+s)
		if err != nil {
			t.Fatal(err)
		}
		if res.L1IPS > Tolerance {
			fails++
			t.Logf("seed %d: L1 %.3f > %.2f", s, res.L1IPS, Tolerance)
		}
	}
	if fails > 0 {
		t.Errorf("%d of %d seeds outside tolerance", fails, seeds)
	}
}

// TestOffPolicyRanksPoliciesLikeTruth is claim (c): the IPS estimate orders
// the current weights and the learned weights as the simulator does.
func TestOffPolicyRanksPoliciesLikeTruth(t *testing.T) {
	cfg := SimConfig{}
	agree := 0
	for s := uint64(0); s < seeds; s++ {
		res, err := run(cfg, studySlates, 100+s, 500+s)
		if err != nil {
			t.Fatal(err)
		}
		if res.SignIPS {
			agree++
		}
		if !res.SignDR {
			t.Logf("seed %d: DR order disagrees with truth (true %.3f vs %.3f; DR %.3f vs %.3f)",
				s, res.TrueLearned, res.TrueCur, res.DRLearned, res.DRCur)
		}
	}
	if agree < seeds {
		t.Errorf("IPS ordering agreed with truth on %d of %d seeds", agree, seeds)
	}
}

// TestSampleSizeCurve is claim (d). It reports how the two claims hold as the
// number of logged slates grows, and writes the table when IPS_OUT is set.
func TestSampleSizeCurve(t *testing.T) {
	cfg := SimConfig{}
	type row struct {
		Slates, Exposures int
		TolPass, SignPass int
		MedL1             float64
	}
	var rows []row
	for _, n := range []int{250, 1000, 4000, 16000} {
		var l1s []float64
		r := row{Slates: n}
		for s := uint64(0); s < seeds; s++ {
			res, err := run(cfg, n, 100+s, 500+s)
			if err != nil {
				t.Fatalf("n=%d seed %d: %v", n, s, err)
			}
			r.Exposures = res.Exposures
			l1s = append(l1s, res.L1IPS)
			if res.L1IPS <= Tolerance {
				r.TolPass++
			}
			if res.SignIPS {
				r.SignPass++
			}
		}
		sort.Float64s(l1s)
		r.MedL1 = l1s[len(l1s)/2]
		rows = append(rows, r)
	}
	for _, r := range rows {
		t.Logf("slates=%d exposures=%d: L1<=%.2f in %d/%d seeds (median L1 %.3f); IPS order right in %d/%d",
			r.Slates, r.Exposures, Tolerance, r.TolPass, seeds, r.MedL1, r.SignPass, seeds)
	}
	if out := os.Getenv("IPS_OUT"); out != "" {
		b, _ := json.MarshalIndent(rows, "", "  ")
		if err := os.WriteFile(filepath.Join(out, "sample_size.json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// TestStudyTable writes the per-seed results, for the report, when IPS_OUT is
// set. It asserts nothing.
func TestStudyTable(t *testing.T) {
	out := os.Getenv("IPS_OUT")
	if out == "" {
		t.Skip("set IPS_OUT to write the study table")
	}
	var all []seedResult
	for s := uint64(0); s < seeds; s++ {
		res, err := run(SimConfig{}, studySlates, 100+s, 500+s)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, res)
	}
	b, _ := json.MarshalIndent(all, "", "  ")
	if err := os.WriteFile(filepath.Join(out, "study.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stderr, "wrote %d seeds to %s\n", len(all), out)
}
