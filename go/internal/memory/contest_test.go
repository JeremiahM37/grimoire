package memory

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

func TestContestedSeparatesModifiersAndVariants(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		// Modifier of a shared head: two true facts about two subjects.
		{"the billing postgres replica lags 2 seconds", "the analytics postgres replica lags 40 seconds", true},
		// Variant tokens with digits are kept: bf16 and fp32 are different things.
		{"the embedding job with bf16 weights runs at 41 tokens per second",
			"the embedding job with fp32 weights runs at 12 tokens per second", true},
		// A refinement only adds detail on one side: left to the rules.
		{"the api gateway uses nginx", "the api gateway uses nginx with rate limiting", false},
		// A true update changes only values, which are not discriminating.
		{"the deploy pipeline takes 11 minutes end to end",
			"the deploy pipeline takes 14 minutes end to end", false},
		// Unrelated facts that share measurement vocabulary: the subjects differ,
		// so they are contested (the vocabulary is only what the rules matched on).
		{"the deploy pipeline takes 11 minutes end to end",
			"the nightly backup takes 20 minutes end to end", true},
		// Same text: not contested.
		{"Dana prefers tabs", "Dana prefers tabs.", false},
	}
	for _, c := range cases {
		if got := Contested(c.a, c.b); got != c.want {
			t.Errorf("Contested(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDiscriminatingIgnoresValuesAndFiller(t *testing.T) {
	got := Discriminating("the billing postgres replica lags 2 seconds, 9:30 am and three retries")
	for _, w := range []string{"billing", "postgres", "replica", "lags", "retries"} {
		if !got[w] {
			t.Errorf("%q missing from discriminating set %v", w, got)
		}
	}
	for _, w := range []string{"2", "9", "30", "three", "seconds", "am", "the"} {
		if got[w] {
			t.Errorf("%q should not be discriminating: %v", w, got)
		}
	}
}

// The defect, with the flag off: two true facts about two subjects collide and
// the rules supersede one of them. This test pins that the flag-off path is
// unchanged.
func TestFlagOffRulesStillSupersedeModifierCollision(t *testing.T) {
	restore := exp.Set()
	defer restore()
	d := DecideAs("the analytics postgres replica lags 40 seconds", "", false,
		[]Entry{{ID: "old", Text: "the billing postgres replica lags 2 seconds"}})
	if d.Op != OpUpdate || d.Target != "old" {
		t.Fatalf("flag off: got %+v, want the known UPDATE", d)
	}
	if d.Contested != "" {
		t.Fatalf("flag off must not mark contested: %+v", d)
	}
}

// With the flag on the same pair is deferred: no supersession, both kept, and
// the decision names the candidate so the gate can be asked.
func TestFlagOnDefersContestedSupersession(t *testing.T) {
	restore := exp.Set("cascade")
	defer restore()
	d := DecideAs("the analytics postgres replica lags 40 seconds", "", false,
		[]Entry{{ID: "old", Text: "the billing postgres replica lags 2 seconds"}})
	if d.Op != OpAdd || d.Contested != "old" || d.Target != "" || d.Challenges != "" {
		t.Fatalf("flag on: got %+v, want OpAdd with Contested=old and no target or challenge", d)
	}
}

// An uncontested true update, changing only its value, is not deferred even
// with the flag on.
func TestFlagOnLeavesUncontestedUpdatesAlone(t *testing.T) {
	restore := exp.Set("cascade")
	defer restore()
	d := DecideAs("the deploy pipeline takes 14 minutes end to end", "", false,
		[]Entry{{ID: "old", Text: "the deploy pipeline takes 11 minutes end to end"}})
	if d.Op != OpUpdate || d.Target != "old" || d.Contested != "" {
		t.Fatalf("uncontested update changed under the flag: %+v", d)
	}
}

// A city change is contested under the whole-statement rule, because the city
// is a value word and the rule cannot tell a single-valued attribute from a
// list. The measured cost of that is in docs/lab/cascade.md; this test pins it.
func TestFlagOnDefersACityChangeAsContested(t *testing.T) {
	restore := exp.Set("cascade")
	defer restore()
	d := DecideAs("Dana lives in Austin", "", false,
		[]Entry{{ID: "old", Text: "Dana lives in Denver"}})
	if d.Op != OpAdd || d.Contested != "old" {
		t.Fatalf("city change under the flag: %+v, want deferred", d)
	}
}
