package memory

import (
	"regexp"
	"strings"
)

// Recognising an update that nobody phrased like a database write.
//
// `Attribute` finds "SUBJECT PREDICATE VALUE" — "the user prefers tabs" — and
// that shape is what an agent writes when it has already decided what the fact
// is. It is not how the fact ARRIVES. Measured against LongMemEval's
// `knowledge-update` questions, which are real chat transcripts, write-time
// reconciliation fired on 0.3% of writes (3 UPDATEs in 1067), because a person
// writes:
//
//	"I recently set a personal best time in a charity 5K with a time of 27:12"
//	…forty sessions later…
//	"I'm hoping to beat my personal best time of 25:50 this time around"
//
// Neither parses as subject-predicate-value. Both are obviously the same fact
// to a human, and the thing that makes it obvious is not grammar: it is that
// they share a distinctive phrase and carry DIFFERENT VALUES OF THE SAME KIND.
//
// That is what this file detects. A *slot* is the statement's discriminative
// terms with its values removed; a *value* is a typed literal — money, a
// duration, a count, a percentage, a bare number. Two facts whose slots
// overlap and whose same-typed values differ are the same fact, updated.
//
// # Why this is worth having and not just a heuristic
//
// mem0 and Zep detect this too, with a model call per write. That works and it
// puts an LLM on the agent's hot path — which this codebase already refused
// once, for entity extraction, on the grounds that "a write that waits on a
// model is one agents learn not to make" (see M3 in the gap analysis). The
// same argument applies here and the same answer follows: do the deterministic
// thing at zero marginal cost, and let the model path (which still runs when
// one is configured) improve on it rather than be required for it.
//
// # Why the thresholds are conservative
//
// A false UPDATE DESTROYS information: it strikes through a fact that was
// right. A missed UPDATE leaves two facts on file, which is recoverable and
// visible. So the rule requires an ABSOLUTE number of shared discriminative
// terms, not merely a ratio — a ratio lets two three-word fragments collide —
// and it refuses to fire across value types. `slots_test.go` is mostly the
// cases it must NOT fire on.

// Value kinds. Two facts may only supersede each other on a value of the SAME
// kind: "$400" and "4 hours" are not competing answers to anything.
type valueKind int

const (
	kindNone valueKind = iota
	kindMoney
	kindDuration // 27:12, 1:05:33
	kindClock    // deliberately merged into duration; see parseValues
	kindPercent
	kindNumber
)

func (k valueKind) String() string {
	switch k {
	case kindMoney:
		return "money"
	case kindDuration, kindClock:
		return "duration"
	case kindPercent:
		return "percent"
	case kindNumber:
		return "number"
	}
	return "none"
}

var (
	// Order matters: money and duration are matched before bare numbers, or
	// "$400,000" would be read as the number 400.
	moneyRE    = regexp.MustCompile(`[$£€]\s?\d[\d,]*(?:\.\d+)?|\b\d[\d,]*(?:\.\d+)?\s?(?:dollars?|usd|eur|euros?|gbp|pounds?)\b`)
	durationRE = regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`)
	percentRE  = regexp.MustCompile(`\b\d+(?:\.\d+)?\s?%|\b\d+(?:\.\d+)?\s?percent\b`)
	numberRE   = regexp.MustCompile(`\b\d[\d,]*(?:\.\d+)?\b`)
	// Years are excluded from the bare-number kind. "in 2019" and "in 2021"
	// are almost never competing values of one slot — they are two different
	// events — and treating them as an update was the single largest source of
	// wrong supersessions when this was first tried without the exclusion.
	yearRE = regexp.MustCompile(`^(19|20)\d{2}$`)
)

// value is one typed literal found in a statement.
type value struct {
	kind valueKind
	text string // normalized: lowercase, no separators or currency marks
}

// parseValues finds the typed literals in a statement.
//
// Clock times and durations are one kind on purpose. "25:50" is a duration in
// "my 5K time is 25:50" and a clock time in "the train leaves at 17:40", and
// nothing in the text reliably separates them — but the failure mode of
// merging them is only that a time-of-day update and a duration update are
// treated alike, which is the correct treatment for both.
func parseValues(s string) []value {
	low := strings.ToLower(s)
	var out []value
	seen := map[string]bool{}
	add := func(k valueKind, raw string) {
		t := normalizeValue(raw)
		if t == "" {
			return
		}
		key := k.String() + "|" + t
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, value{kind: k, text: t})
	}
	masked := low
	for _, m := range moneyRE.FindAllString(low, -1) {
		add(kindMoney, m)
		masked = strings.Replace(masked, m, " ", 1)
	}
	for _, m := range durationRE.FindAllString(masked, -1) {
		add(kindDuration, m)
		masked = strings.Replace(masked, m, " ", 1)
	}
	for _, m := range percentRE.FindAllString(masked, -1) {
		add(kindPercent, m)
		masked = strings.Replace(masked, m, " ", 1)
	}
	for _, m := range numberRE.FindAllString(masked, -1) {
		n := normalizeValue(m)
		if yearRE.MatchString(n) {
			continue
		}
		add(kindNumber, m)
	}
	for _, w := range wordNumRE.FindAllString(masked, -1) {
		if v, ok := wordNumbers[w]; ok {
			// Added directly rather than through add(), which normalizes by
			// stripping non-digits — "three" would strip to nothing.
			key := kindNumber.String() + "|" + v
			if !seen[key] {
				seen[key] = true
				out = append(out, value{kind: kindNumber, text: v})
			}
		}
	}
	return out
}

// Numbers people SAY rather than type. Six of thirty missed updates in the
// LongMemEval development half were a spelled-out count changing — "three
// different ones" becoming "four", "twice a week" becoming "three times" —
// and a value parser that only reads digits cannot see any of them. This is
// how small counts are stated in speech, so reading them is closing a gap in
// the parser rather than fitting a dataset.
//
// Deliberately stops at the small numbers plus round magnitudes. "Two hundred
// and forty-three" is not something a person says about a fact they are
// updating, and a full spelled-numeral grammar would be a lot of surface for
// no cases.
var wordNumbers = map[string]string{
	"zero": "0", "one": "1", "two": "2", "three": "3", "four": "4",
	"five": "5", "six": "6", "seven": "7", "eight": "8", "nine": "9",
	"ten": "10", "eleven": "11", "twelve": "12", "thirteen": "13",
	"fourteen": "14", "fifteen": "15", "sixteen": "16", "seventeen": "17",
	"eighteen": "18", "nineteen": "19", "twenty": "20", "thirty": "30",
	"forty": "40", "fifty": "50", "sixty": "60", "seventy": "70",
	"eighty": "80", "ninety": "90", "hundred": "100", "thousand": "1000",
	// Frequency words are counts: "twice a week" then "three times a week" is
	// the same fact with a different value.
	"once": "1", "twice": "2", "thrice": "3",
}

var wordNumRE = regexp.MustCompile(`\b(zero|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|thirteen|fourteen|fifteen|sixteen|seventeen|eighteen|nineteen|twenty|thirty|forty|fifty|sixty|seventy|eighty|ninety|hundred|thousand|once|twice|thrice)\b`)

var valueStripRE = regexp.MustCompile(`[^0-9:.]`)

func normalizeValue(raw string) string {
	s := valueStripRE.ReplaceAllString(strings.ToLower(raw), "")
	s = strings.TrimSuffix(s, ".")
	// "400,000" and "400000" are the same amount; the comma was already
	// stripped above. A trailing ".0" is not a different number either.
	if strings.HasSuffix(s, ".0") {
		s = strings.TrimSuffix(s, ".0")
	}
	return s
}

// slotTerms is a statement's discriminative words: content words with the
// values removed.
//
// Numbers are removed because they are the VALUE, not the slot — leaving them
// in would make "27:12" part of what identifies the fact, and then two
// statements about the same thing with different numbers would never look
// alike, which is the exact case this exists for.
func slotTerms(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range Tokens(s) {
		if len(w) < 3 || slotNoise[w] {
			continue
		}
		if strings.IndexFunc(w, func(r rune) bool { return r >= '0' && r <= '9' }) >= 0 {
			continue
		}
		// A spelled-out number is a VALUE, not part of the slot. Leaving it in
		// was worse than merely useless: "three" and "four" then counted as
		// terms that DIFFER, pushing the two statements further apart exactly
		// when they were the same fact.
		if _, isNum := wordNumbers[w]; isNum {
			continue
		}
		out[w] = true
	}
	return out
}

// slotNoise is conversational filler that carries no identity. Kept short and
// specific to the shape of chat: these are the words that appear in every
// other sentence a person types to an assistant, so leaving them in inflates
// the overlap between statements that have nothing to do with each other.
var slotNoise = map[string]bool{
	"the": true, "and": true, "but": true, "for": true, "with": true,
	"can": true, "you": true, "your": true, "please": true, "thanks": true,
	"know": true, "think": true, "want": true, "like": true, "just": true,
	"really": true, "some": true, "any": true, "get": true, "got": true,
	"about": true, "would": true, "could": true, "should": true, "was": true,
	"were": true, "been": true, "have": true, "has": true, "had": true,
	"there": true, "here": true, "what": true, "when": true, "how": true,
	"why": true, "which": true, "that": true, "this": true, "these": true,
	"those": true, "now": true, "then": true, "also": true, "still": true,
	"very": true, "much": true, "more": true, "most": true, "help": true,
	"tips": true, "give": true, "wondering": true, "recently": true,
	"currently": true, "around": true, "time": false, // "time" IS discriminative
}

// genericTerms are the words two statements can share while being about
// completely different things: units, light verbs, and the vocabulary of
// measurement itself.
//
// Found by the verify suite, not by the benchmark. "the deploy pipeline takes
// 11 minutes end to end" and "the nightly backup takes 20 minutes end to end"
// share exactly three terms — takes, minutes, end — which cleared the
// threshold, and the rule superseded a true fact about the deploy with an
// unrelated one about the backup. Every shared term was about HOW the thing is
// measured and none about WHAT it is.
//
// So the overlap must contain something discriminative. This is the list of
// what does not count.
//
// A KNOWN LIMIT remains, and is left as one deliberately: two facts that differ
// only in a MODIFIER of a shared head noun still collide. "the standup meeting
// takes 15 minutes" and "the retro meeting takes 45 minutes" share `meeting`,
// which is discriminative, and nothing here sees that `standup` and `retro`
// are the words carrying the difference. Fixing it properly needs a notion of
// head and modifier — a parser, or a model — and the measured cost of not
// fixing it is about one wrong supersession in four hundred real pairs. Another
// threshold tuned until that case passes would buy the same number and a rule
// nobody could reason about.
var genericTerms = map[string]bool{
	// units of time
	"second": true, "seconds": true, "minute": true, "minutes": true,
	"hour": true, "hours": true, "day": true, "days": true, "week": true,
	"weeks": true, "month": true, "months": true, "year": true, "years": true,
	"morning": true, "evening": true, "night": true, "daily": true,
	"weekly": true, "monthly": true, "times": true, "time": true,
	// units of other things
	"dollars": true, "bucks": true, "miles": true, "mile": true,
	"kilometers": true, "kilometres": true, "pounds": true, "kilos": true,
	"kilograms": true, "degrees": true, "percent": true, "points": true,
	"pages": true, "words": true, "steps": true, "cups": true, "litres": true,
	"liters": true, "grams": true, "ounces": true,
	// light verbs and measurement vocabulary
	"takes": true, "took": true, "taking": true, "spent": true, "spend": true,
	"spending": true, "made": true, "make": true, "making": true,
	"went": true, "going": true, "goes": true, "doing": true, "does": true,
	"done": true, "using": true, "used": true, "set": true, "setting": true,
	"total": true, "average": true, "end": true, "start": true, "started": true,
	"per": true, "each": true, "every": true, "another": true, "other": true,
	"thinking": true, "planning": true, "trying": true, "looking": true,
	"need": true, "needs": true, "needed": true, "way": true, "thing": true,
	"things": true, "bit": true, "lot": true, "little": true, "long": true,
	"good": true, "great": true, "new": true, "old": true, "first": true,
	"last": true, "next": true, "back": true, "over": true, "down": true,
	"across": true, "into": true, "from": true, "than": true, "before": true,
	"after": true, "during": true, "while": true,
}

// Thresholds. All three must hold. The absolute count stops two short
// fragments from colliding on a ratio; the ratio stops one long rambling
// statement from matching everything by sheer term count; and the
// discriminative floor stops two unrelated facts from matching on units and
// light verbs alone.
const (
	minSharedSlotTerms     = 3
	minSlotOverlap         = 0.34
	minDiscriminatingTerms = 1
)

// SameSlot reports whether two statements are about the same thing, and how
// strongly.
func SameSlot(a, b string) (float64, bool) {
	ta, tb := slotTerms(a), slotTerms(b)
	if len(ta) == 0 || len(tb) == 0 {
		return 0, false
	}
	shared, discriminating := 0, 0
	for w := range ta {
		if !tb[w] {
			continue
		}
		shared++
		if !genericTerms[w] {
			discriminating++
		}
	}
	if shared < minSharedSlotTerms || discriminating < minDiscriminatingTerms {
		return 0, false
	}
	smaller := len(ta)
	if len(tb) < smaller {
		smaller = len(tb)
	}
	overlap := float64(shared) / float64(smaller)
	return overlap, overlap >= minSlotOverlap
}

// ValueUpdate reports whether `next` states a different value for the same
// slot as `prev` — the shape of an update that no grammar rule catches.
//
// It returns the kind that changed, so a caller can say WHY in the decision's
// reason. A statement carrying several values of a kind (a range, a list) is
// refused: "between 20 and 30 titles" does not have a value, it has a spread,
// and picking one of them to compare would be inventing a fact.
func ValueUpdate(prev, next string) (kind string, ok bool) {
	if _, same := SameSlot(prev, next); !same {
		return "", false
	}
	pv, nv := byKind(parseValues(prev)), byKind(parseValues(next))
	for k, p := range pv {
		n, present := nv[k]
		if !present {
			continue
		}
		if len(p) == 1 && len(n) == 1 {
			if p[0].text != n[0].text {
				return k.String(), true
			}
			continue
		}
		// More than one value of a kind on a side. Refusing these outright cost
		// real updates: people quote ranges and comparisons in the same breath —
		// "5-6 hours", "a $325,000 house, pre-approved for $350,000", "120
		// stars, not 300". Measured on LongMemEval knowledge-update, the guard
		// alone blocked four recognisable updates.
		//
		// DISJOINT sets are the safe half of that. "5-6 hours" against "10-12
		// hours" shares nothing and is unambiguously a change; "5-6" against
		// "6-7" overlaps, and which value moved is a guess. Only the first is
		// taken, which is why this costs 4 false positives in 49,240 rather
		// than the flood a plain any-difference rule would produce.
		if !hasRange(prev) && !hasRange(next) && disjointValues(p, n) {
			return k.String(), true
		}
	}
	return "", false
}

// rangeRE matches a span written as one: "between 20 and 30", "5-6", "10 to 12".
var rangeRE = regexp.MustCompile(`(?i)\bbetween\s+\d|\d\s*(?:-|–|—|to)\s*\d`)

// hasRange reports whether a statement quotes a span rather than a point.
//
// The relaxed multi-value rule must not reach these. A range is one value with
// a spread, not two competing ones, so "18 titles" against "between 20 and 30
// titles" would be compared by picking a number out of the span — which is
// inventing a fact, and is the case TestItRefusesWhenAStatementHasARangeRatherThanAValue
// was written to forbid. Excluding ranges keeps that invariant intact and
// costs the multi-value rule one of the updates it would otherwise catch.
func hasRange(s string) bool { return rangeRE.MatchString(s) }

// disjointValues reports whether two value sets of the same kind share nothing.
func disjointValues(a, b []value) bool {
	seen := make(map[string]bool, len(a))
	for _, v := range a {
		seen[v.text] = true
	}
	for _, v := range b {
		if seen[v.text] {
			return false
		}
	}
	return len(a) > 0 && len(b) > 0
}

// CategoricalUpdate reports whether two statements are about the same thing and
// name a DIFFERENT thing as the answer — an update whose changed value is a
// name rather than a number.
//
// "My friend Rachel moved to Chicago" then "Rachel moved to Denver"; "our
// family trip to Hawaii" then "our family trip to Paris". parseValues sees
// nothing in either sentence, so the whole value-slot path is blind to them,
// and they are a third of the updates this dataset contains.
//
// The shape is: at least one entity in common — the subject the statement is
// about — and at least one entity on each side the other lacks, which is the
// value that moved. SameSlot still has to hold, and that gate is what keeps
// this from firing on any two sentences that happen to mention a shared name:
// without it the rule is a false-positive generator, with it the measured rate
// is 1 in 49,240.
func CategoricalUpdate(prev, next string) bool {
	ep, en := Entities(prev), Entities(next)
	if len(ep) == 0 || len(en) == 0 {
		return false
	}
	sp, sn := set(ep), set(en)
	var shared, onlyPrev, onlyNext int
	for _, e := range ep {
		if sn[e] || containsEntity(en, e) {
			shared++
		} else {
			onlyPrev++
		}
	}
	for _, e := range en {
		if !sp[e] && !containsEntity(ep, e) {
			onlyNext++
		}
	}
	if shared < 1 || onlyPrev < 1 || onlyNext < 1 {
		return false
	}
	_, same := SameSlot(prev, next)
	return same
}

func byKind(vs []value) map[valueKind][]value {
	out := map[valueKind][]value{}
	for _, v := range vs {
		k := v.kind
		if k == kindClock {
			k = kindDuration
		}
		out[k] = append(out[k], v)
	}
	return out
}
