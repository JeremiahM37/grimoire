package memory

import (
	"regexp"
	"sort"
	"strings"
)

// Entity extraction — the third retrieval signal.
//
// Semantic similarity and keyword match both degrade on the query a person
// actually asks memory: "what do I know about Priya's team". The embedding
// blurs the name into its neighbourhood and BM25 needs the name spelled the
// same way it was stored. Matching the entities a fact is *about* recovers the
// facts both of those miss, and it is the signal mem0 adds on top of the same
// two this already had.
//
// Extraction is deterministic. An LLM would name entities better, but memory
// is written on the hot path of an agent loop, and a write that waits on a
// model is a write agents learn not to make.

var (
	wikiRE   = regexp.MustCompile(`\[\[([^\]|]+)(?:\|[^\]]*)?\]\]`)
	codeRE   = regexp.MustCompile("`([^`]+)`")
	handleRE = regexp.MustCompile(`[@#]([\p{L}\p{N}_/-]{2,})`)
	// A run of capitalized words: "Strix Halo", "Priya", "Open WebUI". The
	// trailing (?:\s+\p{Lu}...)* is what keeps a multi-word name together —
	// splitting it would make "Halo" an entity of its own and match every
	// unrelated fact containing it.
	properRE  = regexp.MustCompile(`\p{Lu}[\p{L}\p{N}'’-]+(?:\s+\p{Lu}[\p{L}\p{N}'’-]+)*`)
	acronymRE = regexp.MustCompile(`\b[\p{Lu}]{2,}\b`)
	// Identifiers a homelab fact is full of: paths, hosts, ports, versions.
	identRE = regexp.MustCompile(`(?:/[\p{L}\p{N}_.-]+){2,}|\b[\p{L}\p{N}_-]+\.[\p{L}\p{N}_.-]{2,}\b|\b\d{1,3}(?:\.\d{1,3}){3}\b`)
)

// sentenceStart words are capitalized by grammar rather than by being names.
// Without this the first word of every fact becomes an entity, which matches
// everything and therefore ranks nothing.
var sentenceStart = map[string]bool{
	"the": true, "a": true, "an": true, "i": true, "we": true, "they": true,
	"he": true, "she": true, "it": true, "this": true, "that": true,
	"there": true, "these": true, "those": true, "user": true, "when": true,
	"if": true, "after": true, "before": true, "always": true, "never": true,
	"do": true, "don": true, "prefer": true, "prefers": true, "use": true,
	"uses": true, "his": true, "her": true, "their": true, "my": true,
	"our": true, "your": true, "no": true, "not": true, "for": true,
	"in": true, "on": true, "at": true, "and": true, "or": true, "but": true,
	"is": true, "was": true, "are": true, "were": true, "has": true,
	"have": true, "will": true, "would": true, "should": true, "can": true,
	// Discourse openers. "By the way" is the single most common way a person
	// changes subject mid-message, and it was minting "by" as an entity on
	// every one of them.
	"by": true, "so": true, "well": true, "also": true, "just": true,
	"now": true, "then": true, "actually": true, "speaking": true,
	"anyway": true, "oh": true, "let": true, "as": true, "since": true,
}

// clitic splits a contraction so the stoplist can see the word underneath.
//
// properRE keeps the apostrophe inside a token, so "I've" arrived here as one
// string and never matched the "i" already in sentenceStart. The result was
// that "i've", "i'm" and "i'll" were extracted as ENTITIES — and since almost
// every conversational sentence opens with one, they matched each other across
// completely unrelated facts. Measured on LongMemEval knowledge-update, three
// of them accounted for 1,098 of 1,107 spurious entity matches.
//
// That is not only a reconciliation problem: EntityOverlap is the third
// retrieval signal, so every fact beginning "I've" was scoring an entity hit
// against every other one.
var clitic = regexp.MustCompile(`['’](?:ve|m|ll|d|re|s|t)$`)

// isSentenceStart reports whether a candidate is capitalized by grammar rather
// than by being a name.
func isSentenceStart(w string) bool {
	if sentenceStart[w] {
		return true
	}
	return sentenceStart[clitic.ReplaceAllString(w, "")]
}

// Entities names what a fact is about, normalized for comparison.
func Entities(text string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		s = strings.Trim(s, ".,;:!?'\"()[]")
		if len(s) < 2 || seen[s] || isSentenceStart(s) {
			return
		}
		// A bare stopword is never an entity even when capitalized.
		if stopwords[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	for _, m := range wikiRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range codeRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range handleRE.FindAllStringSubmatch(text, -1) {
		add(m[1])
	}
	for _, m := range identRE.FindAllString(text, -1) {
		add(m)
	}
	for _, m := range acronymRE.FindAllString(text, -1) {
		add(m)
	}
	for _, m := range properRE.FindAllString(stripMarkup(text), -1) {
		// A capitalized run that is only a sentence-initial common word is
		// dropped by add(); a run that *starts* with one keeps its tail, so
		// "The Homelab API" yields "homelab api".
		add(trimLeadingCommon(m))
	}
	sort.Strings(out)
	return out
}

// stripMarkup removes the spans already harvested by a more specific rule, so
// the proper-noun pass does not re-report them in a different shape.
func stripMarkup(text string) string {
	text = wikiRE.ReplaceAllString(text, " ")
	text = codeRE.ReplaceAllString(text, " ")
	return handleRE.ReplaceAllString(text, " ")
}

func trimLeadingCommon(run string) string {
	words := strings.Fields(run)
	for len(words) > 0 && isSentenceStart(strings.ToLower(words[0])) {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// EntityOverlap scores how much two entity sets have in common, 0..1. It is
// asymmetric on purpose: a query naming one entity that a fact is about should
// score 1, not 1/n, or every long fact would outrank the short one that is
// actually on the subject.
func EntityOverlap(query, fact []string) float64 {
	if len(query) == 0 || len(fact) == 0 {
		return 0
	}
	f := set(fact)
	hit := 0
	for _, q := range query {
		if f[q] || containsEntity(fact, q) {
			hit++
		}
	}
	return float64(hit) / float64(len(query))
}

// containsEntity matches a query entity against a fact entity it is part of,
// so asking about "priya" finds a fact whose entity is "priya sharma".
func containsEntity(fact []string, q string) bool {
	for _, f := range fact {
		if len(q) >= 3 && strings.Contains(f, q) {
			return true
		}
	}
	return false
}
