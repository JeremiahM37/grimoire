package memory

// Contested pairs: when two statements that look like the same slot may be
// about two different things.
//
// SameSlot keys on shared words. It cannot tell "the billing postgres replica
// lags 2 seconds" from "the analytics postgres replica lags 40 seconds": both
// share `postgres` and `replica`, and the rule that follows supersedes one of
// two true facts. The fix that needs no model is a symmetry test. When EACH
// statement holds a discriminating word the other lacks, the two may be about
// different subjects, and the rule path does not decide between them on
// overlap alone.
//
// What counts as discriminating is deliberately narrow. Values (numbers,
// number words, clock times, units) are the VALUE of a fact, not its subject,
// and filler and measurement vocabulary (slotNoise, genericTerms) are shared
// by unrelated facts. Variant tokens such as "bf16", "fp32" and "v2" are kept:
// they are exactly what separates a bf16 measurement from an fp32 one.
//
// Contested pairs are where the rule path is weakest and where the cascade
// (see cascade.go) is consulted. Behind the `cascade` experiment flag only.

import "strings"

// contestSkip are tokens that look like content but are measurement or time
// vocabulary: "am" and "pm" follow a clock time, and are never a subject.
var contestSkip = map[string]bool{"am": true, "pm": true, "st": true, "nd": true, "rd": true, "th": true}

// Discriminating returns the words of a statement that name what it is about:
// content words with values, measurement vocabulary and filler removed.
func Discriminating(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range Tokens(s) {
		if slotNoise[w] || genericTerms[w] || contestSkip[w] || isValueWord(w) {
			continue
		}
		out[w] = true
	}
	return out
}

// isValueWord reports a token that is a value rather than a subject: all
// digits (after the normalizer has split "9:30" into "9" and "30"), or a
// spelled-out number. Tokens that mix letters and digits ("bf16", "v2") are not
// values.
func isValueWord(w string) bool {
	if _, ok := wordNumbers[w]; ok {
		return true
	}
	for _, r := range w {
		if r < '0' || r > '9' {
			return false
		}
	}
	return w != ""
}

// Contested reports whether each statement holds a discriminating word the
// other lacks. Two statements where one only adds detail to the other are NOT
// contested here: that case is a refinement, and it is left to the rule path
// as it always was.
func Contested(a, b string) bool {
	da, db := contestPair(a, b)
	if len(da) == 0 || len(db) == 0 {
		return false
	}
	onlyA, onlyB := false, false
	for w := range da {
		if !db[w] {
			onlyA = true
			break
		}
	}
	for w := range db {
		if !da[w] {
			onlyB = true
			break
		}
	}
	return onlyA && onlyB && !strings.EqualFold(a, b)
}

// contestPair returns the discriminating sets to compare for a pair: the whole
// statement on each side. Comparing only the subjects of "SUBJECT PREDICATE
// VALUE" statements was measured and rejected: "X is allergic to A" and "X
// lives in Denver" have the same syntax, and the list case needs the value
// compared. Telling a single-valued update from a list member is a semantic
// judgment, so it is left to the gate.
func contestPair(a, b string) (map[string]bool, map[string]bool) {
	return Discriminating(a), Discriminating(b)
}
