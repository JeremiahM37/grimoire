package api

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/conflict"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Tests for the "lineage" experiment. None of them calls t.Parallel: they turn
// the flag on and off, which is process-global.

func lhit(id, agent, text, ev string) index.MemoryHit {
	return index.MemoryHit{Entry: memory.Entry{
		ID: id, Agent: agent, Text: text, Evidence: ev,
		Verification: memory.VerifiedEvidence,
	}}
}

// runDispute adjudicates the first hit against the rest, the way groupDisputes
// does, and returns the scores by id.
func runDispute(t *testing.T, hits ...index.MemoryHit) (disputeSuggestion, map[string]disputeScore) {
	t.Helper()
	byID := map[string]index.MemoryHit{}
	for _, h := range hits {
		byID[h.ID] = h
	}
	sugg, scores := adjudicate(hits[0], hits[1:], nil, byID)
	out := map[string]disputeScore{}
	for _, s := range scores {
		out[s.ID] = s
	}
	return sugg, out
}

// corroborationTerm reads back the [0,1] term from a breakdown: 0 for one
// lineage, 0.5 for two, 1 for three or more.
func corroborationTerm(s disputeScore) float64 {
	return s.Breakdown.Corroboration / conflict.WeightCorroboration
}

func withLineage(t *testing.T, on bool) func() {
	t.Helper()
	if on {
		return exp.Set(lineageFlag)
	}
	return exp.Set()
}

func TestLineageOnePageQuotedByTenAgentsIsOneLineage(t *testing.T) {
	const page = "https://example.test/page"
	var hits []index.MemoryHit
	for i := 0; i < 10; i++ {
		hits = append(hits, lhit(fmt.Sprintf("a%d", i), fmt.Sprintf("ag%d", i), "Billing runs on 5432", page))
	}

	restore := withLineage(t, true)
	_, on := runDispute(t, hits...)
	restore()
	if got := corroborationTerm(on["a0"]); got != 0 {
		t.Fatalf("flag on: ten agents quoting one page -> term %v, want 0 (one lineage)", got)
	}

	restore = withLineage(t, false)
	_, off := runDispute(t, hits...)
	restore()
	if got := corroborationTerm(off["a0"]); got != 1 {
		t.Fatalf("flag off: ten distinct writers -> term %v, want 1 (unchanged behaviour)", got)
	}
}

func TestLineageTwoIndependentPagesAreTwoLineages(t *testing.T) {
	hits := []index.MemoryHit{
		lhit("a1", "x1", "Billing runs on 5432", "https://example.test/pageA"),
		lhit("a2", "x2", "Billing runs on 5432", "https://example.test/pageA"),
		lhit("b1", "y1", "Billing runs on 5432", "https://example.test/pageB"),
		lhit("b2", "y2", "Billing runs on 5432", "https://example.test/pageB"),
	}
	restore := withLineage(t, true)
	defer restore()
	_, on := runDispute(t, hits...)
	if got := corroborationTerm(on["a1"]); got != 0.5 {
		t.Fatalf("four agents on two pages -> term %v, want 0.5 (two lineages)", got)
	}
}

func TestLineageSelfRepetitionIsOneLineage(t *testing.T) {
	// One writer, three declared origins. Flag off counts the origins; flag on
	// counts the lineages, and a writer never corroborates its own claim.
	hits := []index.MemoryHit{
		lhit("s1", "me", "Billing runs on 5432", ""),
		lhit("s2", "me", "Billing runs on 5432", ""),
		lhit("s3", "me", "Billing runs on 5432", ""),
	}
	hits[1].Origin = "https://example.test/p1"
	hits[2].Origin = "https://example.test/p2"

	restore := withLineage(t, true)
	_, on := runDispute(t, hits...)
	restore()
	if got := corroborationTerm(on["s1"]); got != 0 {
		t.Fatalf("flag on: self-repetition -> term %v, want 0 (one lineage)", got)
	}
	restore = withLineage(t, false)
	_, off := runDispute(t, hits...)
	restore()
	if got := corroborationTerm(off["s1"]); got != 1 {
		t.Fatalf("flag off: self-repetition over three origins -> term %v, want 1", got)
	}
}

func TestLineageDerivedSummaryDoesNotCorroborateItsPremise(t *testing.T) {
	premise := lhit("p", "a", "Billing runs on 5432", "https://example.test/src")
	summary := lhit("s", "b", "Billing runs on 5432", "p")
	restore := withLineage(t, true)
	defer restore()
	_, on := runDispute(t, premise, summary)
	if got := corroborationTerm(on["p"]); got != 0 {
		t.Fatalf("derived summary corroborated its premise (term %v, want 0)", got)
	}
	if on["s"].Hops == nil || *on["s"].Hops != 1 {
		t.Fatalf("summary hop depth = %v, want 1 (one derivation from the premise)", on["s"].Hops)
	}
	if on["p"].Hops == nil || *on["p"].Hops != 0 {
		t.Fatalf("verified premise hop depth = %v, want 0", on["p"].Hops)
	}
}

func TestLineageHumanAuthorityStillWins(t *testing.T) {
	human := lhit("h", "me", "Billing runs on 6432", "")
	human.Human = true
	hits := []index.MemoryHit{lhit("c0", "ag0", "Billing runs on 5432", "https://example.test/p")}
	for i := 1; i < 6; i++ {
		hits = append(hits, lhit(fmt.Sprintf("c%d", i), fmt.Sprintf("ag%d", i), "Billing runs on 5432", "https://example.test/p"))
	}
	hits = append(hits, human)

	restore := withLineage(t, true)
	defer restore()
	sugg, on := runDispute(t, hits...)
	if sugg.ID != "h" {
		t.Fatalf("suggestion = %s, want the human fact even against five agents on one page", sugg.ID)
	}
	if on["h"].Hops == nil || *on["h"].Hops != 0 {
		t.Fatal("a human statement must have hop depth 0")
	}
}

// The simulation: a wrong claim is copied by k dependent sources (each agent
// cites the same wrong page, and writes the same text), and a right claim comes
// from two independent pages. Each trial shuffles the stamps, so recency is
// not a fixed winner. The seed is fixed per k, so the numbers reproduce.
func TestLineageSimulationWrongCopiedByKSources(t *testing.T) {
	const trials = 200
	const right = "Billing runs on 5432"
	const wrong = "Billing runs on 6432"
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	rate := func(k int, on bool) (picks, ties int) {
		restore := withLineage(t, on)
		defer restore()
		rng := rand.New(rand.NewSource(int64(1000 + k)))
		stamp := func() string { return base.Add(-time.Duration(rng.Intn(60*24)) * time.Minute).Format(stampLayout) }
		for trial := 0; trial < trials; trial++ {
			r1 := lhit("r1", "src1", right, "https://right.test/a")
			r2 := lhit("r2", "src2", right, "https://right.test/b")
			r1.Stamp, r2.Stamp = stamp(), stamp()
			hits := []index.MemoryHit{r1, r2}
			for i := 1; i <= k; i++ {
				w := lhit(fmt.Sprintf("w%d", i), fmt.Sprintf("copy%d", i), wrong, "https://wrong.test/p")
				w.Stamp = stamp()
				hits = append(hits, w)
			}
			sugg, _ := runDispute(t, hits...)
			switch {
			case sugg.ID == "r1" || sugg.ID == "r2":
				picks++
			case strings.HasPrefix(sugg.ID, "w"):
			default:
				ties++
			}
		}
		return picks, ties
	}

	for k := 1; k <= 10; k++ {
		offRight, _ := rate(k, false)
		onRight, _ := rate(k, true)
		t.Logf("k=%2d  off: right %3d/%d  on: right %3d/%d", k, offRight, trials, onRight, trials)
		if onRight != trials {
			t.Errorf("k=%d flag on picked the right claim %d/%d times, want all", k, onRight, trials)
		}
		if k >= 3 && offRight == trials {
			t.Errorf("k=%d flag off already picks the right claim every time; the simulation is not testing the copy effect", k)
		}
	}
}

func TestLineageFlagOffAddsNoHops(t *testing.T) {
	hits := []index.MemoryHit{
		lhit("a", "x", "Billing runs on 5432", "https://example.test/p"),
		lhit("b", "y", "Billing runs on 6432", "https://example.test/q"),
	}
	restore := withLineage(t, false)
	_, off := runDispute(t, hits...)
	restore()
	for id, s := range off {
		if s.Hops != nil {
			t.Fatalf("flag off: side %s carries hops %d", id, *s.Hops)
		}
		raw, _ := json.Marshal(s)
		if strings.Contains(string(raw), `"hops"`) {
			t.Fatalf("flag off: breakdown JSON carries hops: %s", raw)
		}
	}

	restore = withLineage(t, true)
	defer restore()
	_, on := runDispute(t, hits...)
	if on["a"].Hops == nil {
		t.Fatal("flag on: hops missing from the breakdown")
	}
}

func TestLineageExplainShowsHopsOnlyUnderTheFlag(t *testing.T) {
	premise := lhit("p", "a", "Billing runs on 5432", "https://example.test/src")
	summary := lhit("s", "b", "Billing runs on 5432", "p")
	hits := []index.MemoryHit{premise, summary}

	restore := withLineage(t, false)
	if out := entriesOut(hits, true); out[1].Hops != nil {
		restore()
		t.Fatal("flag off: explain carries hops")
	}
	restore()
	restore = withLineage(t, true)
	defer restore()
	out := entriesOut(hits, true)
	if out[1].Hops == nil || *out[1].Hops != 1 {
		t.Fatalf("flag on: explain hops = %v, want 1 for the summary", out[1].Hops)
	}
	if plain := entriesOut(hits, false); plain[1].Hops != nil {
		t.Fatal("flag on but not an explain request: hops should not appear")
	}
}
