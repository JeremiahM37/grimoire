package memory

import "testing"

// The three changes that took knowledge-update recognition from 20/72 to 28/72
// on LongMemEval, and the guards that keep them from costing precision.
// See benchmarks/longmemeval/REPORT-slots.md.

// Sentence-initial contractions were being extracted as ENTITIES, so every
// statement opening "I've" matched every other one. Three of them accounted
// for 1,098 of 1,107 spurious entity matches — and because EntityOverlap is a
// retrieval signal, this was scoring hits in ranking too, not just here.
func TestContractionsAreNotEntities(t *testing.T) {
	for _, tc := range []struct{ text, junk string }{
		{"I've tried three different Korean restaurants recently", "i've"},
		{"By the way, I'm reading about DNA structure", "i'm"},
		{"I'll check out Teva and Merrell sandals", "i'll"},
		{"By the way, I'd like to visit Kyoto", "i'd"},
	} {
		for _, e := range Entities(tc.text) {
			if e == tc.junk {
				t.Errorf("%q extracted %q as an entity", tc.text, tc.junk)
			}
		}
	}
	// The real names still survive the change.
	got := Entities("I'll check out Teva and Merrell sandals")
	if len(got) != 2 {
		t.Errorf("real entities lost: %v", got)
	}
}

// "By the way" is the commonest way a person changes subject mid-message, and
// it was minting "by" as an entity on every one of them.
func TestDiscourseOpenersAreNotEntities(t *testing.T) {
	for _, e := range Entities("By the way, speaking of Paris, I moved") {
		if e == "by" || e == "speaking" {
			t.Errorf("discourse opener %q became an entity", e)
		}
	}
}

// A value that moved is still an update when the sentence carries more than one
// value of that kind — people quote a comparison in the same breath.
func TestMultipleValuesOfAKindStillUpdate(t *testing.T) {
	if _, ok := ValueUpdate(
		"I need 125 stars to reach the gold level on the rewards program",
		"I need 120 stars to reach the gold level, not 300"); !ok {
		t.Error("a changed count was missed because the new statement also " +
			"quoted the value it was correcting")
	}
}

// Overlapping sets are not a change anyone can name.
func TestOverlappingValueSetsAreNotAnUpdate(t *testing.T) {
	if _, ok := ValueUpdate(
		"the deploy takes 5 or 6 minutes end to end",
		"the deploy takes 6 or 7 minutes end to end"); ok {
		t.Error("overlapping value sets were treated as a change")
	}
}

// The range invariant is older than the multi-value rule and outranks it.
func TestARangeIsStillNotAValue(t *testing.T) {
	if _, ok := ValueUpdate(
		"my to-watch list has 18 titles on it",
		"my to-watch list has between 20 and 30 titles on it"); ok {
		t.Error("a range was treated as a value")
	}
}
