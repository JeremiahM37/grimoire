// Package fts builds SQLite FTS5 MATCH expressions from user input.
//
// It exists because of one invariant (docs/ARCHITECTURE.md #4): user input must
// never reach FTS as syntax. An unquoted NEAR, OR or * in a search box would
// otherwise change the shape of the query — at best surprising results, at
// worst an error the user cannot explain.
//
// Three call sites needed that guarantee and each grew its own escaping, with
// two different conventions for the same hazard. The escaping now lives here
// once; the three POLICIES stay distinct, because they genuinely differ:
//
//	Phrase       one quoted phrase — adjacency required   (live-query blocks)
//	Terms        each word quoted, implicit AND           (memory recall)
//	PrefixTerms  each word quoted + prefix-matched        (/api/search)
//
// Terms exists separately from Phrase because a single phrase was too strict
// for recall: it missed any query whose words were not adjacent in the note.
// PrefixTerms exists because search-as-you-type needs the trailing `*`.
package fts

import "strings"

// escape makes a bare term safe to place inside double quotes. FTS5 escapes a
// literal quote by doubling it, the same rule SQL string literals use.
func escape(term string) string {
	return strings.ReplaceAll(term, `"`, `""`)
}

// quote wraps a term as an exact-match token.
func quote(term string) string { return `"` + escape(term) + `"` }

// Phrase matches the whole input as one adjacent phrase.
func Phrase(text string) string { return quote(text) }

// Terms requires every word, in any position.
func Terms(text string) string {
	fields := strings.Fields(text)
	out := make([]string, len(fields))
	for i, f := range fields {
		out[i] = quote(f)
	}
	return strings.Join(out, " ")
}

// PrefixTerms matches each term as a prefix, joined by op (" " for AND,
// " OR " for the any-term fallback). It returns an empty phrase for no terms,
// which matches nothing rather than erroring.
//
// Note the difference from Terms: a quote inside a term is REMOVED here, not
// doubled. `"foo` as a prefix token would become `"""foo"*` — a phrase
// containing a literal quote, which matches nothing — so for a prefix matcher
// dropping the character is the behaviour that still finds what the user meant.
func PrefixTerms(terms []string, op string) string {
	if len(terms) == 0 {
		return `""`
	}
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = `"` + strings.ReplaceAll(t, `"`, "") + `"*`
	}
	return strings.Join(out, op)
}

// Operators for PrefixTerms.
const (
	And = " "
	Or  = " OR "
)
