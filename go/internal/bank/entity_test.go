package bank

import (
	"testing"
	"time"
)

func TestSeqRatioMatchesRatcliffObershelp(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want float64
	}{
		{"abcd", "bcde", 0.75},
		{"alice", "alice chen", 2 * 5.0 / 15},
		{"", "", 1},
		{"abc", "xyz", 0},
	} {
		if got := seqRatio(c.a, c.b); got < c.want-1e-9 || got > c.want+1e-9 {
			t.Errorf("seqRatio(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestTrigramsPadWordStarts(t *testing.T) {
	tri := trigrams("Al")
	for _, want := range []string{"  a", " al", "al "} {
		if _, ok := tri[want]; !ok {
			t.Errorf("missing %q in %v", want, tri)
		}
	}
	if j := jaccard(trigrams("Alice"), trigrams("ALICE!")); j != 1 {
		t.Errorf("case and punctuation must not matter: %v", j)
	}
}

func TestTokensCompatible(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Alice", "Alice Chen", true},
		{"Alice Chen", "Bob Chen", false},
		{"Room 101", "Room 102", false},
		{"Room 101", "Room 0101", true},
		{"Bob", "Robert", true}, // single words are always compatible; the score decides
		{"Acme Corp", "Acme Corporation", true},
	} {
		if got := tokensCompatible(c.a, c.b); got != c.want {
			t.Errorf("tokensCompatible(%q,%q) = %v", c.a, c.b, got)
		}
	}
}

func TestResolveReusesOnlyWithCorroboration(t *testing.T) {
	now := time.Date(2023, 5, 1, 0, 0, 0, 0, time.UTC)
	known := []knownEntity{
		{Name: "Alice", LastSeen: now, Cooc: map[string]bool{"bob": true}},
		{Name: "Bob", LastSeen: now, Cooc: map[string]bool{"alice": true}},
		{Name: "Room 102"},
	}
	r := newResolver(known)
	got := r.Resolve([]Mention{
		{Text: "alice"},      // same words: reuse
		{Text: "Alice Chen"}, // spelling alone is not enough
		{Text: "Alice  Chen", Nearby: []string{"Bob"}, Event: now}, // company + recency: reuse
		{Text: "Room 101"}, // different number: never
		{Text: "  "},       // empty: dropped
	})
	want := []string{"Alice", "Alice Chen", "Alice", "Room 101", ""}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("mention %d: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewNamesInOneBatchCluster(t *testing.T) {
	r := newResolver(nil)
	got := r.Resolve([]Mention{{Text: "Dr. Smith"}, {Text: "Dr Smith"}, {Text: "dr smith"}, {Text: "Carol"}})
	if got[0] != got[1] || got[1] != got[2] {
		t.Errorf("spellings of one name must share a canonical: %v", got)
	}
	if got[0] != "Dr Smith" {
		t.Errorf("canonical = %q; ties go to the shortest spelling", got[0])
	}
	if got[3] != "Carol" {
		t.Errorf("unrelated name changed: %q", got[3])
	}
}

func TestLiteralMentionsAreNeverFuzzyMerged(t *testing.T) {
	r := newResolver([]knownEntity{{Name: "Alice", Cooc: map[string]bool{}}})
	got := r.Resolve([]Mention{{Text: "ALICE", Literal: true}, {Text: "Alicia", Literal: true}})
	if got[0] != "Alice" || got[1] != "Alicia" {
		t.Errorf("got %v", got)
	}
}

func TestEntityIDIsCaseInsensitiveAndStable(t *testing.T) {
	if EntityID("b", "Alice") != EntityID("b", "alice") || EntityID("b", "Alice") == EntityID("c", "Alice") {
		t.Error("entity ids must be per bank and case-insensitive")
	}
}

func TestRuleEntities(t *testing.T) {
	got := ruleEntities("Yesterday I met Alice Chen and Bob in New York. The trip was in May. Thanks!")
	want := map[string]bool{"Alice Chen": true, "Bob": true, "New York": true}
	for _, g := range got {
		if !want[g] {
			t.Errorf("unexpected entity %q in %v", g, got)
		}
	}
	for _, w := range []string{"Alice Chen", "Bob", "New York"} {
		found := false
		for _, g := range got {
			found = found || g == w
		}
		if !found {
			t.Errorf("missing %q in %v", w, got)
		}
	}
}
