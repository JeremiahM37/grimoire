package memory

import (
	"sort"
	"strings"
	"unicode"
)

// Deterministic query expansion helpers for recall.
//
// Nothing here calls a model. Recall expansion builds a handful of cheap
// variants of the query (entities only, keywords only, alias-resolved,
// suffix-stripped) and the index runs each through the ordinary ranking. The
// functions below only produce those variant strings, so they are pure and
// cheap enough to run on every recall that asks for expansion.

// expansionStop are question and filler words that carry no retrieval signal
// in a recall query. They are removed from the keyword variant only; the
// literal query is never rewritten, so a stopword-sensitive query still gets
// its original ranking.
var expansionStop = map[string]bool{
	"what": true, "which": true, "who": true, "whom": true, "whose": true,
	"when": true, "where": true, "why": true, "how": true, "does": true,
	"did": true, "do": true, "done": true, "doing": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "been": true,
	"about": true, "know": true, "tell": true, "me": true, "we": true,
	"you": true, "our": true, "my": true, "your": true, "their": true,
	"his": true, "her": true, "its": true, "any": true, "some": true,
	"there": true, "have": true, "has": true, "had": true, "got": true,
	"think": true, "remember": true, "recall": true, "please": true,
	"with": true, "from": true, "into": true, "over": true, "under": true,
	"just": true, "also": true, "then": true, "than": true, "such": true,
}

// ExpansionKeywords returns the content words of a query: tokenised and
// lowercased by Tokens, with question and filler words removed.
func ExpansionKeywords(query string) []string {
	var out []string
	for _, w := range Tokens(query) {
		if !expansionStop[w] {
			out = append(out, w)
		}
	}
	return out
}

// ExpansionEntities returns the entities of a query that are worth searching
// for on their own. Entity extraction is capitalisation-driven, so a query's
// leading question word can come back as an "entity"; those are dropped here.
func ExpansionEntities(query string) []string {
	var out []string
	for _, e := range Entities(query) {
		if !expansionStop[e] && len(e) >= 2 {
			out = append(out, e)
		}
	}
	return out
}

// EntityAliases folds a lone first name into the one full name that starts
// with it: when the names set holds "dana" and exactly one multi-word name
// beginning "dana " ("dana kim"), "dana" is an alias of "dana kim". Two full
// names sharing the first word leave the first name alone, because guessing
// which person is meant would weld two people together.
//
// This is the same rule bank/entity.go applies to a bank's names. It is
// duplicated rather than imported because the bank package sits above this
// one and the rule is six lines. The input is lowercase entities; the output
// maps each alias to its full name, also lowercase. Names are read in sorted
// order so the result does not depend on the order they were gathered in.
func EntityAliases(names []string) map[string]string {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)

	single := map[string]bool{}
	byFirst := map[string][]string{}
	seen := map[string]bool{}
	for _, n := range sorted {
		low := strings.ToLower(strings.TrimSpace(n))
		if low == "" || seen[low] {
			continue
		}
		seen[low] = true
		ws := strings.Fields(low)
		switch {
		case len(ws) == 1 && isLetters(ws[0]) && len([]rune(ws[0])) >= 2:
			single[ws[0]] = true
		case len(ws) >= 2 && len(ws) <= 4:
			byFirst[ws[0]] = append(byFirst[ws[0]], low)
		}
	}
	out := map[string]string{}
	for s := range single {
		if cands := byFirst[s]; len(cands) == 1 {
			out[s] = cands[0]
		}
	}
	return out
}

func isLetters(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return s != ""
}

// StemVariant returns the crude suffix-stripped form of a lowercase word, or
// the word itself when no rule applies. It handles the inflections that keep
// a term from matching across tenses and plurals in a recall query — "owns"
// and "owned" to "own", "deploying" to "deploy" — and nothing more. It is not
// a stemmer: an unknown word passes through unchanged rather than being
// mangled, which is why every rule needs a minimum length.
func StemVariant(w string) string {
	n := len(w)
	switch {
	case n > 5 && strings.HasSuffix(w, "ing"):
		return undouble(w[:n-3])
	case n > 4 && strings.HasSuffix(w, "ied"):
		return w[:n-3] + "y"
	case n > 4 && strings.HasSuffix(w, "ed") && !strings.HasSuffix(w, "eed"):
		return undouble(w[:n-2])
	case n > 4 && strings.HasSuffix(w, "ies"):
		return w[:n-3] + "y"
	case n > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss"):
		return w[:n-1]
	}
	return w
}

// undouble drops a doubled final consonant left by "-ing" and "-ed" stripping:
// "runn" to "run", "stopp" to "stop". Doubled l, s and z are kept because
// "call", "pass" and "buzz" are their own stems.
func undouble(s string) string {
	n := len(s)
	if n < 3 || s[n-1] != s[n-2] {
		return s
	}
	switch s[n-1] {
	case 'l', 's', 'z':
		return s
	}
	if strings.ContainsRune("aeiou", rune(s[n-1])) {
		return s
	}
	return s[:n-1]
}

// FirstNameAliases resolves the words of a query, not the stored names. A
// query word that is nobody's entity on its own ("priya") still resolves when
// exactly one stored full name starts with it ("priya sharma"). EntityAliases
// cannot see this case: it only folds a lone name that was itself stored.
// The same uniqueness rule applies, so an ambiguous first name stays put.
func FirstNameAliases(words []string, names []string) map[string]string {
	byFirst := map[string][]string{}
	seen := map[string]bool{}
	for _, n := range names {
		low := strings.ToLower(strings.TrimSpace(n))
		ws := strings.Fields(low)
		if len(ws) >= 2 && len(ws) <= 4 && !seen[low] {
			seen[low] = true
			byFirst[ws[0]] = append(byFirst[ws[0]], low)
		}
	}
	out := map[string]string{}
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if _, done := out[w]; done || !isLetters(w) || len([]rune(w)) < 2 || expansionStop[w] {
			continue
		}
		if cands := byFirst[w]; len(cands) == 1 {
			out[w] = cands[0]
		}
	}
	return out
}
