package memory

import "strings"

// Stem reduces a lowercase word to a rough root by stripping one common English
// suffix. It is deliberately conservative: it only has to make "linter" meet
// "lint" and "migrations" meet "migration" when a prompt's wording differs
// from a memory's, and a wrong merge costs only a slightly looser overlap score.
// It is not used when facts are written or reconciled, so stored text and
// supersede decisions are unaffected.
func Stem(w string) string {
	if len(w) < 5 {
		return w
	}
	for _, rule := range stemRules {
		if strings.HasSuffix(w, rule.suffix) && len(w)-len(rule.suffix) >= 3 {
			w = w[:len(w)-len(rule.suffix)] + rule.repl
			break
		}
	}
	// A trailing silent e is dropped so "configure" meets "configures".
	if len(w) >= 5 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}

// StemTokens is Tokens with every word stemmed.
func StemTokens(s string) []string {
	toks := Tokens(s)
	for i, t := range toks {
		toks[i] = Stem(t)
	}
	return toks
}

// stemRules are tried in order, longest and most specific first.
var stemRules = []struct{ suffix, repl string }{
	{"ations", "at"}, {"ation", "at"}, {"ings", ""}, {"ing", ""},
	{"ions", ""}, {"ion", ""}, {"ments", ""}, {"ment", ""},
	{"ers", ""}, {"er", ""}, {"ies", "y"}, {"ied", "y"},
	{"ed", ""}, {"ly", ""}, {"es", ""}, {"s", ""},
}
