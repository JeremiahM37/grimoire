// Package grounded answers questions from dated records the way a careful
// person would: gather every relevant dated passage first, answer from that
// list, then check the answer against the sources.
//
// It has three parts that work alone or together:
//
//   - temporal.go: relative time expressions in a record ("last Saturday",
//     "two weeks ago") are annotated with the absolute date they denote,
//     computed from that record's own timestamp. The annotation is stored
//     beside the text and never replaces it.
//   - timeline.go: dated events per person/entity, extracted once when a
//     corpus is ingested (by the memory bank's extractor) and queried for
//     list/count questions.
//   - answer.go: the three-step gather / answer / self-check procedure, over
//     either the whole text or retrieved passages.
//
// Nothing here knows about any benchmark. Inputs are records with timestamps.
package grounded

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Annotation is one relative time expression resolved to an absolute date or
// range. Start/End are byte offsets of the expression in the original text.
type Annotation struct {
	Start  int       `json:"start"`
	End    int       `json:"end"`
	Phrase string    `json:"phrase"`
	Text   string    `json:"text"` // e.g. "= Sat 20 May 2023" or "~ April 2023"
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Approx bool      `json:"approx,omitempty"`
	// Model is true when a model, not the rules, resolved an ambiguous phrase.
	Model bool `json:"model,omitempty"`
}

// Resolver settles phrases the rules cannot (a bare "on Saturday" with no
// tense to say whether it is last or next Saturday). It is optional; without
// one such phrases are simply left unannotated.
type Resolver interface {
	Resolve(ctx context.Context, sentence, phrase string, ref time.Time) (from, to time.Time, ok bool)
}

func dayOf(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func fmtDay(t time.Time) string { return t.Format("Mon 2 Jan 2006") }

func fmtRange(a, b time.Time) string {
	if a.Equal(b) {
		return fmtDay(a)
	}
	if a.Year() == b.Year() && a.Month() == b.Month() {
		return a.Format("Mon 2") + " – " + b.Format("Mon 2 Jan 2006")
	}
	return fmtDay(a) + " – " + fmtDay(b)
}

var wordNum = map[string]int{
	"a": 1, "an": 1, "one": 1, "two": 2, "three": 3, "four": 4, "five": 5, "six": 6, "seven": 7,
	"eight": 8, "nine": 9, "ten": 10, "eleven": 11, "twelve": 12, "couple of": 2, "couple": 2,
}

const numRE = `(\d{1,3}|an?|one|two|three|four|five|six|seven|eight|nine|ten|eleven|twelve|(?:a )?couple(?: of)?)`

func num(s string) (n int, approx bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if strings.Contains(s, "couple") {
		return 2, true
	}
	if v, err := strconv.Atoi(s); err == nil {
		return v, false
	}
	return wordNum[s], false
}

var weekdays = map[string]time.Weekday{
	"monday": time.Monday, "tuesday": time.Tuesday, "wednesday": time.Wednesday, "thursday": time.Thursday,
	"friday": time.Friday, "saturday": time.Saturday, "sunday": time.Sunday,
}

const weekdayRE = `(monday|tuesday|wednesday|thursday|friday|saturday|sunday)`
const seasonRE = `(spring|summer|autumn|fall|winter)`

// mondayOf is the Monday of t's ISO week.
func mondayOf(t time.Time) time.Time {
	t = dayOf(t)
	return t.AddDate(0, 0, -((int(t.Weekday()) + 6) % 7))
}

// lastWeekday is the most recent given weekday strictly before ref.
func lastWeekday(ref time.Time, w time.Weekday) time.Time {
	d := dayOf(ref).AddDate(0, 0, -1)
	for d.Weekday() != w {
		d = d.AddDate(0, 0, -1)
	}
	return d
}

// nextWeekday is the first given weekday strictly after ref.
func nextWeekday(ref time.Time, w time.Weekday) time.Time {
	d := dayOf(ref).AddDate(0, 0, 1)
	for d.Weekday() != w {
		d = d.AddDate(0, 0, 1)
	}
	return d
}

func weekdayOfWeek(ref time.Time, w time.Weekday) time.Time {
	m := mondayOf(ref)
	return m.AddDate(0, 0, (int(w)+6)%7)
}

// pastWeekend returns the nth most recent weekend (n>=1) that ended before ref.
func pastWeekend(ref time.Time, n int) (time.Time, time.Time) {
	sat := lastWeekday(ref, time.Saturday)
	// If ref is Sunday, that Saturday's weekend is still in progress.
	if dayOf(ref).Weekday() == time.Sunday {
		sat = sat.AddDate(0, 0, -7)
	}
	sat = sat.AddDate(0, 0, -7*(n-1))
	return sat, sat.AddDate(0, 0, 1)
}

func monthShift(ref time.Time, k int) (time.Time, time.Time) {
	first := time.Date(ref.Year(), ref.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, k, 0)
	return first, first.AddDate(0, 1, -1)
}

func seasonRange(name string, year int) (time.Time, time.Time) {
	u := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	switch name {
	case "spring":
		return u(year, 3, 1), u(year, 5, 31)
	case "summer":
		return u(year, 6, 1), u(year, 8, 31)
	case "autumn", "fall":
		return u(year, 9, 1), u(year, 11, 30)
	}
	return u(year, 12, 1), u(year+1, 3, 1).AddDate(0, 0, -1) // winter starting in `year`
}

// lastSeason is the most recent season that ended before ref.
func lastSeason(name string, ref time.Time) (time.Time, time.Time) {
	ref = dayOf(ref)
	for y := ref.Year(); y >= ref.Year()-2; y-- {
		a, b := seasonRange(name, y)
		if b.Before(ref) {
			// walk forward: there may be a later complete one
			if a2, b2 := seasonRange(name, y+1); b2.Before(ref) {
				return a2, b2
			}
			return a, b
		}
	}
	return seasonRange(name, ref.Year()-1)
}

type span struct {
	from, to time.Time
	text     string
	approx   bool
}

type rule struct {
	re *regexp.Regexp
	fn func(m []string, ref time.Time) (span, bool)
}

func r(s string) *regexp.Regexp { return regexp.MustCompile(`(?i)\b` + s + `\b`) }

func exact(d time.Time) span  { return span{from: d, to: d, text: "= " + fmtDay(d)} }
func rng(a, b time.Time) span { return span{from: a, to: b, text: "= " + fmtRange(a, b)} }
func approxDay(d time.Time) span {
	return span{from: d, to: d, text: "≈ " + fmtDay(d), approx: true}
}

func unitAgo(n int, unit string, sign int, ref time.Time, approx bool) (span, bool) {
	ref = dayOf(ref)
	if n <= 0 || n > 400 {
		return span{}, false
	}
	switch strings.TrimSuffix(strings.ToLower(unit), "s") {
	case "day":
		d := ref.AddDate(0, 0, sign*n)
		if approx {
			return approxDay(d), true
		}
		return exact(d), true
	case "week":
		return approxDay(ref.AddDate(0, 0, sign*7*n)), true
	case "month":
		a, b := monthShift(ref, sign*n)
		return span{from: a, to: b, text: "≈ " + a.Format("January 2006"), approx: true}, true
	case "year":
		y := ref.Year() + sign*n
		return span{from: time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), to: time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC),
			text: "≈ " + strconv.Itoa(y), approx: true}, true
	}
	return span{}, false
}

var rules = []rule{
	{r(`the day before yesterday`), func(m []string, ref time.Time) (span, bool) { return exact(dayOf(ref).AddDate(0, 0, -2)), true }},
	{r(`the day after tomorrow`), func(m []string, ref time.Time) (span, bool) { return exact(dayOf(ref).AddDate(0, 0, 2)), true }},
	{r(`yesterday`), func(m []string, ref time.Time) (span, bool) { return exact(dayOf(ref).AddDate(0, 0, -1)), true }},
	{r(`tomorrow`), func(m []string, ref time.Time) (span, bool) { return exact(dayOf(ref).AddDate(0, 0, 1)), true }},
	{r(`last night`), func(m []string, ref time.Time) (span, bool) {
		d := dayOf(ref).AddDate(0, 0, -1)
		return span{from: d, to: d, text: "= night of " + fmtDay(d)}, true
	}},
	{r(`tonight`), func(m []string, ref time.Time) (span, bool) {
		d := dayOf(ref)
		return span{from: d, to: d, text: "= night of " + fmtDay(d)}, true
	}},
	{r(`today|this (?:morning|afternoon|evening)`), func(m []string, ref time.Time) (span, bool) { return exact(dayOf(ref)), true }},
	{r(numRE + ` (days?|weeks?|months?|years?) ago`), func(m []string, ref time.Time) (span, bool) {
		n, ap := num(m[1])
		return unitAgo(n, m[2], -1, ref, ap)
	}},
	{r(`in ` + numRE + ` (days?|weeks?|months?|years?)(?: from now| time)?`), func(m []string, ref time.Time) (span, bool) {
		n, ap := num(m[1])
		return unitAgo(n, m[2], +1, ref, ap)
	}},
	{r(`(last|this past) ` + weekdayRE), func(m []string, ref time.Time) (span, bool) {
		return exact(lastWeekday(ref, weekdays[strings.ToLower(m[2])])), true
	}},
	{r(`next ` + weekdayRE), func(m []string, ref time.Time) (span, bool) {
		return exact(nextWeekday(ref, weekdays[strings.ToLower(m[1])])), true
	}},
	{r(`this ` + weekdayRE), func(m []string, ref time.Time) (span, bool) {
		return exact(weekdayOfWeek(ref, weekdays[strings.ToLower(m[1])])), true
	}},
	{r(numRE + ` weekends? ago`), func(m []string, ref time.Time) (span, bool) {
		n, ap := num(m[1])
		if n < 1 || n > 20 {
			return span{}, false
		}
		a, b := pastWeekend(ref, n)
		s := rng(a, b)
		if ap {
			s.text, s.approx = "≈ "+fmtRange(a, b), true
		}
		return s, true
	}},
	{r(`(last|this|next) weekend`), func(m []string, ref time.Time) (span, bool) {
		switch strings.ToLower(m[1]) {
		case "last":
			a, b := pastWeekend(ref, 1)
			return rng(a, b), true
		case "this":
			a := weekdayOfWeek(ref, time.Saturday)
			return rng(a, a.AddDate(0, 0, 1)), true
		}
		a := weekdayOfWeek(ref, time.Saturday).AddDate(0, 0, 7)
		s := rng(a, a.AddDate(0, 0, 1))
		s.text, s.approx = "≈ "+fmtRange(a, a.AddDate(0, 0, 1)), true
		return s, true
	}},
	{r(`(last|this|next) week`), func(m []string, ref time.Time) (span, bool) {
		k := map[string]int{"last": -1, "this": 0, "next": 1}[strings.ToLower(m[1])]
		a := mondayOf(ref).AddDate(0, 0, 7*k)
		return rng(a, a.AddDate(0, 0, 6)), true
	}},
	{r(`(last|next) month`), func(m []string, ref time.Time) (span, bool) {
		k := -1
		if strings.EqualFold(m[1], "next") {
			k = 1
		}
		a, b := monthShift(ref, k)
		return span{from: a, to: b, text: "= " + a.Format("January 2006")}, true
	}},
	{r(`(last|next) year`), func(m []string, ref time.Time) (span, bool) {
		y := ref.Year() - 1
		if strings.EqualFold(m[1], "next") {
			y = ref.Year() + 1
		}
		return span{from: time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC), to: time.Date(y, 12, 31, 0, 0, 0, 0, time.UTC), text: "= " + strconv.Itoa(y)}, true
	}},
	{r(`last ` + seasonRE), func(m []string, ref time.Time) (span, bool) {
		a, b := lastSeason(strings.ToLower(m[1]), ref)
		return span{from: a, to: b, text: "= " + fmtRange(a, b)}, true
	}},
}

// ambiguous: a bare weekday ("on Saturday") is last or next Saturday
// depending on the sentence's tense.
var bareWeekday = regexp.MustCompile(`(?i)\bon ` + weekdayRE + `\b`)

var futureCue = regexp.MustCompile(`(?i)\b(will|won't|gonna|going to|planning|plan to|plans|upcoming|looking forward|can't wait|cannot wait|excited (?:for|about|to)|hoping|want to|would|should|let's|shall|i'll|we'll|they'll|he'll|she'll|supposed to|scheduled|about to|next)\b|'ll\b`)
var pastCue = regexp.MustCompile(`(?i)\b(was|were|went|had|did|got|took|saw|made|ran|visited|attended|finished|started|joined|celebrated|bought|met|came|played|ate|won|lost|ended|began|last|ago|yesterday)\b|\b\w{3,}ed\b`)

func sentenceAround(text string, i, j int) string {
	a := strings.LastIndexAny(text[:i], ".!?\n") + 1
	b := strings.IndexAny(text[j:], ".!?\n")
	if b < 0 {
		b = len(text)
	} else {
		b += j
	}
	return strings.TrimSpace(text[a:b])
}

// Annotate resolves the relative time expressions in text against ref (the
// timestamp of the record that holds them). Annotations are returned in text
// order and never overlap.
func Annotate(text string, ref time.Time) []Annotation {
	anns, _ := AnnotateWith(context.Background(), text, ref, nil)
	return anns
}

// AnnotateWith is Annotate plus an optional model for ambiguous phrases.
func AnnotateWith(ctx context.Context, text string, ref time.Time, res Resolver) ([]Annotation, int) {
	if ref.IsZero() {
		return nil, 0
	}
	var cands []Annotation
	for _, ru := range rules {
		for _, idx := range ru.re.FindAllStringSubmatchIndex(text, -1) {
			m := make([]string, len(idx)/2)
			for k := range m {
				if idx[2*k] >= 0 {
					m[k] = text[idx[2*k]:idx[2*k+1]]
				}
			}
			s, ok := ru.fn(m, ref)
			if !ok {
				continue
			}
			cands = append(cands, Annotation{Start: idx[0], End: idx[1], Phrase: m[0], Text: s.text, From: s.from, To: s.to, Approx: s.approx})
		}
	}
	asked := 0
	for _, idx := range bareWeekday.FindAllStringSubmatchIndex(text, -1) {
		wd := weekdays[strings.ToLower(text[idx[2]:idx[3]])]
		sent := sentenceAround(text, idx[0], idx[1])
		fut, past := futureCue.MatchString(sent), pastCue.MatchString(sent)
		var d time.Time
		var model bool
		switch {
		case fut && !past:
			d = nextWeekday(ref, wd)
			if dayOf(ref).Weekday() == wd {
				d = dayOf(ref)
			}
		case past && !fut:
			d = lastWeekday(ref, wd)
		case res != nil:
			asked++
			from, _, ok := res.Resolve(ctx, sent, text[idx[0]:idx[1]], ref)
			if !ok {
				continue
			}
			d, model = dayOf(from), true
		default:
			continue
		}
		cands = append(cands, Annotation{Start: idx[0], End: idx[1], Phrase: text[idx[0]:idx[1]], Text: "= " + fmtDay(d), From: d, To: d, Model: model})
	}
	// Longest-first non-overlapping selection, then text order.
	sort.SliceStable(cands, func(i, j int) bool {
		li, lj := cands[i].End-cands[i].Start, cands[j].End-cands[j].Start
		if li != lj {
			return li > lj
		}
		return cands[i].Start < cands[j].Start
	})
	var out []Annotation
	for _, c := range cands {
		clash := false
		for _, o := range out {
			if c.Start < o.End && o.Start < c.End {
				clash = true
				break
			}
		}
		if !clash {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })
	return out, asked
}

// Render writes text with each annotation appended after its expression:
// "last Saturday [= Sat 20 May 2023]". The original words are untouched.
func Render(text string, anns []Annotation) string {
	if len(anns) == 0 {
		return text
	}
	var b strings.Builder
	pos := 0
	for _, a := range anns {
		if a.Start < pos || a.End > len(text) {
			continue
		}
		b.WriteString(text[pos:a.End])
		fmt.Fprintf(&b, " [%s]", a.Text)
		pos = a.End
	}
	b.WriteString(text[pos:])
	return b.String()
}

// AnnotateText is Annotate followed by Render.
func AnnotateText(text string, ref time.Time) string { return Render(text, Annotate(text, ref)) }
