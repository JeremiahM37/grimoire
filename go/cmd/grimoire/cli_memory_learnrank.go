package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/rankfit"
)

// memoryLearnRankCmd fits the ranking weights offline on the exposure log and
// reports them against the hand-set ones. It is read-only: it opens the
// adherence database without migrating it, and it writes nothing, not even the
// weights. Deploying them is a separate decision (docs/lab/ips.md).
func memoryLearnRankCmd(e *env, args []string) int {
	days, err := strconv.Atoi(flagOr(args, "--days", "0"))
	if err != nil || days < 0 {
		return fail("--days must be a non-negative integer")
	}
	var since time.Time
	if days > 0 {
		since = time.Now().AddDate(0, 0, -days)
	}
	dir := filepath.Join(e.vault.Root, ".grimoire")
	rep := learnRankReport{Hand: index.HandRankWeights()}
	var rows []adherence.ExposureRow
	if _, err := os.Stat(filepath.Join(dir, "adherence.db")); os.IsNotExist(err) {
		rep.Status = "no exposure log yet"
	} else {
		db, err := adherence.OpenReadOnly(dir)
		if err != nil {
			return fail("open adherence log read-only: %v", err)
		}
		defer db.Close()
		var counts adherence.ExposureCounts
		rows, counts, err = adherence.ReadExposures(db, since)
		if err != nil {
			return fail("read exposures: %v", err)
		}
		rep.Counts = counts
	}
	slates := exposureSlates(rows)
	rep.UsableSlates = len(slates)
	for _, s := range slates {
		rep.UsableExposures += len(s)
	}
	switch {
	case rep.Status != "":
	case len(slates) == 0:
		rep.Status = "no usable slates: positions and features are logged only under the ips experiment"
	default:
		learnRankFit(&rep, slates)
	}
	if hasFlag(args, "--json") {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	printLearnRank(rep)
	return 0
}

type learnRankReport struct {
	Counts          adherence.ExposureCounts `json:"counts"`
	UsableSlates    int                      `json:"usable_slates"`
	UsableExposures int                      `json:"usable_exposures"`
	Status          string                   `json:"status,omitempty"`
	Exam            []float64                `json:"exam,omitempty"`
	ExamN           []int                    `json:"exam_n,omitempty"`
	Hand            [index.RankDims]float64  `json:"hand"`
	Learned         []weightCI               `json:"learned,omitempty"`
	Naive           []weightCI               `json:"naive,omitempty"`
	Policies        *policyCompare           `json:"policies,omitempty"`
}

type weightCI struct {
	Feature string  `json:"feature"`
	Weight  float64 `json:"weight"`
	Lo      float64 `json:"lo"`
	Hi      float64 `json:"hi"`
}

type policyCompare struct {
	Slates     int     `json:"test_slates"`
	IPSHand    float64 `json:"ips_hand"`
	IPSLearned float64 `json:"ips_learned"`
	DRHand     float64 `json:"dr_hand"`
	DRLearned  float64 `json:"dr_learned"`
	Note       string  `json:"note"`
}

// exposureSlates groups logged rows by slate. A slate is usable when every item
// has a position and a full feature vector; anything else is dropped rather than
// guessed at.
func exposureSlates(rows []adherence.ExposureRow) [][]rankfit.Item {
	order := []string{}
	bySlate := map[string][]rankfit.Item{}
	bad := map[string]bool{}
	for _, r := range rows {
		if r.Slate == "" {
			continue
		}
		var feat []float64
		if err := json.Unmarshal([]byte(r.Feats), &feat); err != nil || len(feat) != index.RankDims || r.Pos < 1 {
			bad[r.Slate] = true
		}
		if _, ok := bySlate[r.Slate]; !ok {
			order = append(order, r.Slate)
		}
		ps := 1 - r.PWithhold
		if ps <= 0 {
			ps = 1
		}
		bySlate[r.Slate] = append(bySlate[r.Slate], rankfit.Item{Slate: r.Slate, Pos: r.Pos,
			Explored: r.Explored, Shown: r.Shown, PShow: ps, Used: boolFloat(r.Used), Feat: feat})
	}
	var out [][]rankfit.Item
	for _, id := range order {
		if !bad[id] {
			out = append(out, bySlate[id])
		}
	}
	return out
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func learnRankFit(rep *learnRankReport, slates [][]rankfit.Item) {
	const reps = 200
	var items []rankfit.Item
	k := 0
	for _, s := range slates {
		items = append(items, s...)
		if len(s) > k {
			k = len(s)
		}
	}
	ex, err := rankfit.EstimateExam(items, k, rankfit.Options{})
	if err != nil {
		rep.Status = "position bias not estimable: " + err.Error()
		rep.ExamN = ex.N
		return
	}
	rep.Exam, rep.ExamN = ex.Curve, ex.N
	learned, err := rankfit.FitPropensity(items, ex.Curve, rankfit.Options{})
	if err != nil {
		rep.Status = "fit failed: " + err.Error()
		return
	}
	naive, _ := rankfit.FitNaive(items, rankfit.Options{})
	lo, hi, err := rankfit.Bootstrap(slates, ex.Curve, false, rankfit.Options{}, reps, 1)
	if err == nil {
		rep.Learned = weightRows(learned, lo, hi)
	}
	if nlo, nhi, err := rankfit.Bootstrap(slates, nil, true, rankfit.Options{}, reps, 2); err == nil && naive != nil {
		rep.Naive = weightRows(naive, nlo, nhi)
	}
	rep.Policies = comparePolicies(slates, k)
}

// comparePolicies fits on even slates and evaluates the learned and hand-set
// weights on odd ones, so the learned policy is not scored on its own data.
func comparePolicies(slates [][]rankfit.Item, k int) *policyCompare {
	var train, test [][]rankfit.Item
	for i, s := range slates {
		if i%2 == 0 {
			train = append(train, s)
		} else {
			test = append(test, s)
		}
	}
	var trItems []rankfit.Item
	for _, s := range train {
		trItems = append(trItems, s...)
	}
	ex, err := rankfit.EstimateExam(trItems, k, rankfit.Options{})
	if err != nil {
		return nil
	}
	w, err := rankfit.FitPropensity(trItems, ex.Curve, rankfit.Options{})
	if err != nil || len(test) == 0 {
		return nil
	}
	handW := index.HandRankWeights()
	hand := handW[:]
	pc := &policyCompare{Slates: len(test),
		IPSHand: rankfit.EvaluateIPS(test, ex.Curve, hand), IPSLearned: rankfit.EvaluateIPS(test, ex.Curve, w)}
	pc.DRHand, _ = rankfit.EvaluateDR(test, ex.Curve, hand, rankfit.Options{})
	pc.DRLearned, _ = rankfit.EvaluateDR(test, ex.Curve, w, rankfit.Options{})
	pc.Note = "values are in units of position-1 examination; a difference is only meaningful with its interval, which this command does not yet report"
	return pc
}

func weightRows(w, lo, hi []float64) []weightCI {
	out := make([]weightCI, len(w))
	for i := range w {
		out[i] = weightCI{Feature: rankfit.FeatureNames[i], Weight: w[i], Lo: lo[i], Hi: hi[i]}
	}
	return out
}

func printLearnRank(rep learnRankReport) {
	c := rep.Counts
	fmt.Printf("exposure log: %d shown, %d withheld; %d positioned (%d slates), %d with features, %d explored, %d cited\n",
		c.Injections, c.Withheld, c.Positioned, c.Slates, c.WithFeatures, c.Explored, c.Cited)
	if c.Note != "" {
		fmt.Println("note:", c.Note)
	}
	fmt.Printf("usable: %d slates, %d exposures\n", rep.UsableSlates, rep.UsableExposures)
	if rep.Status != "" {
		fmt.Println("status:", rep.Status)
		return
	}
	fmt.Printf("examination (position 1 = 1): %v (n per position %v)\n", rep.Exam, rep.ExamN)
	fmt.Println("feature        hand    learned [95% CI]           naive [95% CI]")
	for i, f := range rankfit.FeatureNames {
		l := rep.Learned[i]
		n := rep.Naive[i]
		fmt.Printf("%-14s %6.3f  %6.3f [%6.3f, %6.3f]   %6.3f [%6.3f, %6.3f]\n",
			f, rep.Hand[i], l.Weight, l.Lo, l.Hi, n.Weight, n.Lo, n.Hi)
	}
	if p := rep.Policies; p != nil {
		fmt.Printf("off-policy (test half, %d slates): IPS hand %.4f vs learned %.4f; DR hand %.4f vs learned %.4f\n",
			p.Slates, p.IPSHand, p.IPSLearned, p.DRHand, p.DRLearned)
	}
}
