package index

import "github.com/JeremiahM37/grimoire/go/internal/memory"

// Exact-phrase scoring for recall.
//
// At 50,000 facts the dominant recall miss is a full-name query ("what does
// Priya A. Schmidt own") whose target is outranked by facts about other people
// who share the first and last name and differ only in the middle initial
// ("Priya N. Schmidt works in UTC"). The keyword arm cannot separate them: each
// shares every query word except the initial, and the entity arm cannot either,
// because entity extraction drops single-letter words and entity overlap is a
// substring test that matches "priya" and "schmidt" in both.
//
// phraseScore rewards the one signal that does separate them: how long a run of
// the query's words appears contiguously and in the same order in the fact. A
// run of one word is common and says nothing about identity, so only runs of
// two or more count. Words are memory.Tokens, the same tokens keyword scoring
// uses, so the comparison is deterministic and needs no model.

// wPhrase weights the phrase score inside the base score. It is one fixed
// value chosen before measurement and not tuned against the benchmark.
const wPhrase = 0.05

// phraseScore is the longest contiguous run of qTokens found in the fact's
// tokens, as a fraction of the query's tokens; zero for a run shorter than two.
func phraseScore(qTokens []string, text string) float64 {
	if len(qTokens) < 2 {
		return 0
	}
	fTokens := memory.Tokens(text)
	run := longestRun(qTokens, fTokens)
	if run < 2 {
		return 0
	}
	return float64(run) / float64(len(qTokens))
}

// longestRun is the length of the longest contiguous subsequence of q that also
// occurs contiguously in f. Queries and facts are a few dozen words, so the
// quadratic scan is cheap next to the vector work already done per candidate.
func longestRun(q, f []string) int {
	best := 0
	for i := range q {
		for j := range f {
			n := 0
			for i+n < len(q) && j+n < len(f) && q[i+n] == f[j+n] {
				n++
			}
			if n > best {
				best = n
			}
		}
	}
	return best
}
