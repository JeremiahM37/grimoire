package memory

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// Reconciliation — deciding what a new fact does to what is already known.
//
// Appending every fact an agent reports is the behaviour that makes long-lived
// memory useless: "prefers tabs" and "prefers spaces" both sit in the note,
// recall returns whichever ranks higher, and the agent acts on a belief the
// user corrected months ago. Reconciliation runs at write time and decides,
// per fact, whether it is new (ADD), replaces something (UPDATE), retracts
// something (DELETE), or says nothing not already recorded (NOOP).
//
// The decision is made by an LLM when one is configured and by the rules here
// when one is not. The rules are not a degraded mode: they cover the shape
// that actually recurs — an attribute of a subject changing value — and they
// are deterministic, which is what lets the behaviour be tested at all.
//
// There are TWO rule paths, and the second exists because the first was
// measured and found to miss almost everything real:
//
//	Attribute   "SUBJECT PREDICATE VALUE" — the shape an agent writes once it
//	            has decided what the fact is
//	ValueUpdate same discriminative terms, different value of the same kind —
//	            the shape a fact ARRIVES in when a person states it twice,
//	            months apart, in two different sentences
//
// On LongMemEval's knowledge-update transcripts the first path fired on 0.8%
// of writes. See slots.go.

// Op is what a fact does to the existing set.
type Op string

const (
	OpAdd    Op = "ADD"    // nothing on file covers it
	OpUpdate Op = "UPDATE" // supersedes a specific entry
	OpDelete Op = "DELETE" // retracts a specific entry, adding nothing
	OpNoop   Op = "NOOP"   // already known
)

// Decision is the outcome for one candidate fact.
type Decision struct {
	Op     Op
	Text   string // the fact, as it should be stored (empty for DELETE/NOOP)
	Target string // ID of the entry being superseded or retracted
	Why    string // human-readable reason, surfaced in the API response
}

// stopwords are dropped before comparing facts. The list is deliberately
// short: a longer one starts discarding words that carry the difference
// between two beliefs ("not", "no", "never" are the obvious trap and are kept).
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "to": true, "in": true,
	"on": true, "at": true, "for": true, "and": true, "or": true, "is": true,
	"are": true, "was": true, "were": true, "be": true, "been": true,
	"that": true, "this": true, "it": true, "as": true, "with": true,
}

var punctRE = regexp.MustCompile(`[^\p{L}\p{N}\s]+`)
var spaceRE = regexp.MustCompile(`\s+`)

// Normalize reduces a fact to the form two equivalent statements share:
// lowercase, no punctuation, single-spaced.
func Normalize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = punctRE.ReplaceAllString(s, " ")
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

// Tokens is Normalize split into content words.
func Tokens(s string) []string {
	var out []string
	for _, w := range strings.Fields(Normalize(s)) {
		if !stopwords[w] {
			out = append(out, w)
		}
	}
	return out
}

// Similarity is the Jaccard overlap of two facts' content words, 0..1.
func Similarity(a, b string) float64 {
	ta, tb := set(Tokens(a)), set(Tokens(b))
	if len(ta) == 0 || len(tb) == 0 {
		return 0
	}
	inter := 0
	for w := range ta {
		if tb[w] {
			inter++
		}
	}
	union := len(ta) + len(tb) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

func set(words []string) map[string]bool {
	m := make(map[string]bool, len(words))
	for _, w := range words {
		m[w] = true
	}
	return m
}

// predicates are the verbs that make a sentence an assignment of a value to an
// attribute of a subject — the shape whose *value* changes over time and whose
// old value must therefore stop being recalled.
var predicates = []string{
	"prefers", "prefer", "likes", "dislikes", "uses", "using", "runs",
	"lives in", "works at", "works on", "is called", "is named", "wants",
	"needs", "owns", "has", "avoids", "hates", "loves", "drives", "reads",
	"writes", "speaks", "believes", "chose", "chooses", "picked", "picks",
	"is", "was",
}

// negations retract rather than replace: they say the old value is wrong and
// offer none in its place.
var negations = []string{
	"no longer", "not anymore", "no more", "stopped", "quit", "gave up",
	"doesn't", "does not", "doesnt", "isn't", "is not", "isnt", "never again",
}

// Attribute splits a fact into the subject, the predicate, and the value
// assigned. Two facts that share a subject and predicate but disagree on the
// value are the case reconciliation exists for.
func Attribute(text string) (subject, predicate, value string, ok bool) {
	n := Normalize(text)
	// Longest predicate first, so "works at" is not read as "is".
	best := -1
	var bestPred string
	for _, p := range predicates {
		idx := indexWord(n, p)
		if idx < 0 {
			continue
		}
		if best < 0 || idx < best || (idx == best && len(p) > len(bestPred)) {
			best, bestPred = idx, p
		}
	}
	if best < 0 {
		return "", "", "", false
	}
	subject = trimArticles(n[:best])
	value = trimArticles(n[best+len(bestPred):])
	if subject == "" || value == "" {
		return "", "", "", false
	}
	return subject, bestPred, value, true
}

// articles are dropped from a subject and a value so that "the user prefers
// tabs" and "user prefers tabs" are one belief rather than two. Possessives
// ("my", "her") are deliberately NOT dropped: they carry whose cat it is, and
// merging two people's facts is a worse failure than keeping a duplicate.
var articles = map[string]bool{"the": true, "a": true, "an": true}

func trimArticles(s string) string {
	words := strings.Fields(strings.TrimSpace(s))
	for len(words) > 0 && articles[words[0]] {
		words = words[1:]
	}
	return strings.Join(words, " ")
}

// indexWord finds a phrase on word boundaries, so "is" does not match inside
// "this".
func indexWord(hay, needle string) int {
	from := 0
	for {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return -1
		}
		i += from
		startOK := i == 0 || hay[i-1] == ' '
		end := i + len(needle)
		endOK := end == len(hay) || hay[end] == ' '
		if startOK && endOK {
			return i
		}
		from = i + 1
	}
}

// IsNegation reports whether a fact retracts rather than states.
func IsNegation(text string) bool {
	n := Normalize(text)
	for _, neg := range negations {
		if indexWord(n, Normalize(neg)) >= 0 {
			return true
		}
	}
	return false
}

// Thresholds for the rule path. Duplicate is high because superseding a fact
// that merely resembles another loses information; contradiction is decided by
// subject+predicate agreement rather than by similarity, which is why the
// numbers here can afford to be conservative.
const (
	duplicateThreshold = 0.9
	relatedThreshold   = 0.45
)

// Decide works out what one new fact does to the entries already on file.
// Candidates should be the entries retrieval considered closest; the caller
// decides how many, and passing all of them is correct for a small vault.
func Decide(fact string, candidates []Entry) Decision {
	return DecideFrom(fact, "", candidates)
}

// DecideFrom is Decide for a fact with a known origin.
//
// The rule it adds is one line and is the whole point: a fact whose source is
// text other people can write MAY NOT supersede or retract a fact that came
// from the operator. Without it, the memory engine is the most attractive
// target in the system — an agent that reads a poisoned Jira comment and
// dutifully remembers "the deploy host is now evil.example" does not merely
// answer one question wrongly, it overwrites the true fact, strikes it
// through, and every later recall returns the attacker's version. Reconciling
// is a WRITE, and a write is exactly what untrusted text must not be able to
// direct.
//
// It is deliberately the same shape as Immutable: an untrusted fact is not
// discarded — discarding it would lose information an operator might want to
// see — it is added ALONGSIDE, where a person and a reader can both see the
// two claims disagree. Silence would be worse than a contradiction.
func DecideFrom(fact, origin string, candidates []Entry) Decision {
	if strings.TrimSpace(fact) == "" {
		return Decision{Op: OpNoop, Why: "empty"}
	}
	untrusted := trust.FromOrigin(origin) == trust.Untrusted
	negated := IsNegation(fact)
	subject, predicate, value, structured := Attribute(fact)

	var (
		bestSim   float64
		bestEntry Entry
		haveBest  bool
	)
	for _, c := range candidates {
		if c.Superseded() {
			// A belief already replaced cannot be replaced again, and must not
			// be the thing a new fact supersedes — that would rewrite history
			// rather than extend it.
			continue
		}
		sim := Similarity(fact, c.Text)

		// Same attribute of the same subject, different value: the fact
		// changed. This is checked before similarity because the two texts can
		// be lexically distant ("prefers tabs" vs "prefers four-space
		// indentation") and still be the same belief being overwritten.
		if structured && !c.Immutable && !blocks(untrusted, c) {
			cs, cp, cv, cok := Attribute(c.Text)
			if cok && cs == subject && cp == predicate {
				if cv == value && !negated {
					return Decision{Op: OpNoop, Target: c.ID,
						Why: "already recorded: " + c.Text}
				}
				if negated {
					return Decision{Op: OpDelete, Target: c.ID,
						Why: "retracts: " + c.Text}
				}
				return Decision{Op: OpUpdate, Text: fact, Target: c.ID,
					Why: "supersedes: " + c.Text}
			}
		}
		// The value-slot path. Tried AFTER Attribute, so a canonical fact
		// keeps taking the precise route, and before the similarity
		// fallback, because "same slot, different number" is a stronger
		// signal than lexical overlap and means the opposite thing: two
		// statements that share most of their words but differ in the one
		// number are a correction, not a duplicate.
		if !negated && !c.Immutable && !blocks(untrusted, c) {
			if kind, ok := ValueUpdate(c.Text, fact); ok {
				return Decision{Op: OpUpdate, Text: fact, Target: c.ID,
					Why: "supersedes (" + kind + " value changed): " + c.Text}
			}
		}
		if sim > bestSim {
			bestSim, bestEntry, haveBest = sim, c, true
		}
	}
	if !haveBest {
		return Decision{Op: OpAdd, Text: fact, Why: "nothing related on file"}
	}
	switch {
	case bestSim >= duplicateThreshold && !negated:
		return Decision{Op: OpNoop, Target: bestEntry.ID,
			Why: "already recorded: " + bestEntry.Text}
	case negated && bestSim >= relatedThreshold && !bestEntry.Immutable &&
		!blocks(untrusted, bestEntry):
		return Decision{Op: OpDelete, Target: bestEntry.ID,
			Why: "retracts: " + bestEntry.Text}
	case untrusted && blocks(untrusted, bestEntry):
		// Recorded, not silently dropped, and the reason is in the response so
		// a caller can surface the disagreement rather than discovering later
		// that two contradictory facts are both being recalled.
		return Decision{Op: OpAdd, Text: fact,
			Why: "recorded alongside (untrusted source may not supersede): " + bestEntry.Text}
	default:
		return Decision{Op: OpAdd, Text: fact, Why: "nothing related on file"}
	}
}

// blocks reports whether an untrusted new fact is forbidden from superseding
// this existing entry.
//
// Untrusted-over-untrusted is allowed: a connector re-pulling a document that
// has since been edited should update what it said before, and refusing that
// would make every re-sync accumulate a contradiction. What is refused is a
// stranger's text overwriting the operator's.
func blocks(untrusted bool, existing Entry) bool {
	return untrusted && !existing.Untrusted()
}

// sentenceRE splits on sentence terminators followed by whitespace. Clause
// splitting ("and", ";") is deliberately not done: over-splitting produces
// fragments that lose their subject, and a fragment that recalls without its
// subject is worse than a sentence that is slightly too long.
var sentenceRE = regexp.MustCompile(`(?m)[.!?]+\s+|\n+`)

// ExtractFacts breaks a remembered blob into individually-reconcilable facts.
// A single sentence — the overwhelmingly common case — comes back as itself.
func ExtractFacts(text string) []string {
	var out []string
	for _, part := range sentenceRE.Split(text, -1) {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "- "))
		part = strings.TrimRight(part, ".")
		if part == "" {
			continue
		}
		out = append(out, part)
	}
	if len(out) == 0 {
		if t := strings.TrimSpace(text); t != "" {
			return []string{t}
		}
	}
	return out
}
