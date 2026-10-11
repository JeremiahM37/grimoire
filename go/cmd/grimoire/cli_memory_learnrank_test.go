package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/rankfit"
)

// TestLearnRankWithoutALogSaysSo checks that a vault with no exposure log gets a
// status line, not an error, and that the command writes nothing.
func TestLearnRankWithoutALogSaysSo(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "memory", "learn-rank", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var rep learnRankReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if rep.Status != "no exposure log yet" || rep.UsableSlates != 0 {
		t.Fatalf("report %+v", rep)
	}
}

// TestExposureSlatesDropsIncompleteSlates checks that a slate with a missing
// feature vector is discarded rather than fitted with a guess.
func TestExposureSlatesDropsIncompleteSlates(t *testing.T) {
	good := `[0.1,0.2,0.3,0.4,0.5,0,1]`
	rows := []adherence.ExposureRow{
		{Slate: "a", Pos: 1, Shown: true, Feats: good},
		{Slate: "a", Pos: 2, Shown: true, Feats: good},
		{Slate: "b", Pos: 1, Shown: true, Feats: good},
		{Slate: "b", Pos: 2, Shown: true, Feats: ""},
		{Slate: "", Pos: 1, Shown: true, Feats: good},
	}
	slates := exposureSlates(rows)
	if len(slates) != 1 || len(slates[0]) != 2 {
		t.Fatalf("got %d slates; want only slate a", len(slates))
	}
}

// TestLearnRankFitOnPlantedLog runs the fit path on a simulated log and checks
// that the propensity-weighted weights point the planted way.
func TestLearnRankFitOnPlantedLog(t *testing.T) {
	sim := rankfit.Simulate(rankfit.SimConfig{}, 16000, 42)
	slates := sim.Slates
	var rep learnRankReport
	learnRankFit(&rep, slates)
	if rep.Status != "" {
		t.Fatalf("status %q", rep.Status)
	}
	w := make([]float64, len(rep.Learned))
	for i, l := range rep.Learned {
		w[i] = l.Weight
	}
	if d := rankfit.L1(w, rankfit.TruthWeights); d > rankfit.Tolerance {
		t.Fatalf("learned weights %v are %.3f from the planted ones", w, d)
	}
	if rep.Policies == nil || rep.Policies.Slates == 0 {
		t.Fatal("no off-policy comparison")
	}
	if !strings.Contains(rep.Policies.Note, "interval") {
		t.Fatal("off-policy note should say intervals are not reported")
	}
}
