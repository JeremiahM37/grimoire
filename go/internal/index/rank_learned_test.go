package index

import (
	"math"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// rankedScores returns the scores of the entries for a query, in rank order.
func rankedScores(t *testing.T, ix *Index, query string) []MemoryHit {
	t.Helper()
	hits, err := ix.MemoryEntries(MemoryQuery{Limit: 10, Query: query})
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func seedRankNote(t *testing.T) *Index {
	t.Helper()
	ix := testIndex(t)
	memNote(t, ix, "memory/prefs.md",
		entry("e1", "2026-08-14 09:00", "user prefers tabs for the server config"),
		entry("e2", "2026-08-15 09:00", "the server runs proxmox"),
		entry("e3", "2026-08-16 09:00", "user runs a backup every night"))
	return ix
}

// TestLearnedWeightsAreIgnoredWithTheFlagOff checks that a stored vector has no
// effect unless the ips experiment is on.
func TestLearnedWeightsAreIgnoredWithTheFlagOff(t *testing.T) {
	ix := seedRankNote(t)
	base := rankedScores(t, ix, "server backup")
	if err := ix.setMeta(rankWeightsKey, `{"weights":[0,0,0,1,0,0,0]}`); err != nil {
		t.Fatal(err)
	}
	restore := exp.Set()
	defer restore()
	after := rankedScores(t, ix, "server backup")
	if len(after) != len(base) {
		t.Fatalf("flag off: %d hits after storing weights, %d before", len(after), len(base))
	}
	for i := range base {
		// Recency reads the clock, so scores drift by a few ulps between calls.
		if base[i].ID != after[i].ID || math.Abs(base[i].Score-after[i].Score) > 1e-9 {
			t.Fatalf("flag off changed ranking at %d: %s %.17g vs %s %.17g", i, base[i].ID, base[i].Score, after[i].ID, after[i].Score)
		}
	}
}

// TestLearnedWeightsApplyUnderTheFlag checks that a stored recency-only vector
// reorders by recency plus the phrase term, and that with no stored vector the
// flag changes nothing.
func TestLearnedWeightsApplyUnderTheFlag(t *testing.T) {
	ix := seedRankNote(t)
	base := rankedScores(t, ix, "server backup")
	restore := exp.Set(ipsFlag)
	defer restore()
	noVector := rankedScores(t, ix, "server backup")
	for i := range base {
		if math.Abs(base[i].Score-noVector[i].Score) > 1e-9 || base[i].ID != noVector[i].ID {
			t.Fatalf("flag on without stored weights changed score of %s", base[i].ID)
		}
	}
	if err := ix.setMeta(rankWeightsKey, `{"weights":[0,0,0,1,0,0,0]}`); err != nil {
		t.Fatal(err)
	}
	got := rankedScores(t, ix, "server backup")
	for _, h := range got {
		want := RankFeatures(h)[3] + wPhrase*phraseScore(memory.Tokens("server backup"), h.Text)
		if math.Abs(h.Score-want) > 1e-12 {
			t.Fatalf("%s: score %.6f, want recency + phrase %.6f", h.ID, h.Score, want)
		}
	}
}

// TestHandWeightsMatchTheConstants keeps the exported comparison vector in step
// with the constants it reports.
func TestHandWeightsMatchTheConstants(t *testing.T) {
	w := HandRankWeights()
	if w[0] != wSemantic || w[1] != wKeyword || w[2] != wEntity || w[3] != wRecency || w[4] != wUseful || w[5] != 0 || w[6] != 0 {
		t.Fatalf("HandRankWeights %v does not match the constants", w)
	}
}
