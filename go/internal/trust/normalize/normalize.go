// Package normalize produces the forms of a line that a reader would act on,
// so the injection scanner can look at what a model will see rather than at
// the bytes a person typed.
//
// A scanner that matches "ignore all previous instructions" against raw text
// is defeated by anything that changes the bytes but not the meaning: a
// zero-width space inside a word, a Cyrillic "о" in place of a Latin "o", a
// fullwidth "Ｉ", a non-breaking space between words, a base64 run that a
// model decodes on sight, or the same sentence inside an HTML comment that a
// markdown renderer hides. Each of those is a canonicalisation or a reveal
// step here, and every variant a line yields keeps the list of steps that
// produced it, so a finding can say what exposed the match.
//
// The forms are deterministic and offline. Nothing here asks a model whether a
// string is an instruction; it only produces candidate readings for the
// existing pattern set to judge. That keeps the defence measurable: the
// benchmark in benchmarks/injection reports recall and false positives from
// the same code the vault runs.
//
// Bounds are part of the contract. Decoding is limited to a fixed depth, a
// fixed number of variants per line and a fixed decoded size per run, so a
// hostile line cannot make the scan expensive. A decoded run is kept only when
// it is valid UTF-8 that reads as text; random binary decoded from a hash or a
// blob is not a reading of anything.
package normalize

import (
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Variant is one reading of a line. Text is the canonical form; Transforms are
// the steps applied to reach it, in order, e.g. ["base64", "zero-width"]. The
// raw line is always the first variant, with no transforms.
type Variant struct {
	Text       string
	Transforms []string
}

// Limits. They are exported so the tests can probe the edges.
const (
	// MaxDepth bounds how many reveal or decode steps are chained.
	MaxDepth = 2
	// MaxVariants bounds the variants produced for one line.
	MaxVariants = 48
	// MaxInput is the longest line that is decoded at all. Longer lines are
	// still canonicalised, which is linear, but no decoder runs on them.
	MaxInput = 8192
	// MaxDecoded is the largest decoded output kept for one run.
	MaxDecoded = 4096
)

// Expand returns every reading of line the scanner should judge: the raw line,
// its canonical form, and the readings produced by decoding and revealing.
// Variants are deduplicated on text, keeping the first chain that produced it.
func Expand(line string) []Variant {
	e := &expander{seen: map[string]bool{}}
	e.add(line, nil)
	// Decoding always starts from the canonical form, even when it equals the
	// raw line and was therefore not added again.
	c, cs := Canonical(line)
	e.add(c, cs)
	e.walk(c, cs, 0)
	return e.out
}

type expander struct {
	seen map[string]bool
	out  []Variant
}

func (e *expander) add(text string, transforms []string) bool {
	if e.seen[text] || len(e.out) >= MaxVariants {
		return false
	}
	e.seen[text] = true
	e.out = append(e.out, Variant{Text: text, Transforms: transforms})
	return true
}

func (e *expander) walk(text string, chain []string, depth int) {
	if depth >= MaxDepth || len(text) > MaxInput {
		return
	}
	for _, step := range steps {
		out, ok := step.apply(text)
		if !ok {
			continue
		}
		next := append(append([]string(nil), chain...), step.name)
		c, cs := Canonical(out)
		full := append(next, cs...)
		if e.add(c, full) {
			e.walk(c, full, depth+1)
		}
	}
}

// Canonical folds a line to the form a reader sees: invisible characters are
// removed, compatibility forms are mapped to their plain letters, common
// lookalike letters are mapped to Latin, and runs of whitespace collapse to one
// space. It returns the transforms that actually changed the text.
func Canonical(s string) (string, []string) {
	var ts []string
	t := stripInvisible(s)
	if t != s {
		ts = append(ts, "zero-width")
	}
	n := norm.NFKC.String(t)
	if n != t {
		ts = append(ts, "nfkc")
	}
	h := foldHomoglyphs(n)
	if h != n {
		ts = append(ts, "homoglyph")
	}
	w := collapseSpace(h)
	if w != h {
		ts = append(ts, "whitespace")
	}
	return w, ts
}

// invisible reports code points a reader does not see but a tokenizer does.
// U+00AD (soft hyphen), U+034F (combining grapheme joiner), U+061C, U+180E and
// the tag block are included alongside the zero-width and bidi controls that
// the secscan hidden-character check already names.
func invisible(r rune) bool {
	switch {
	case r >= 0x200B && r <= 0x200F, r >= 0x202A && r <= 0x202E,
		r >= 0x2060 && r <= 0x2069, r == 0xFEFF, r == 0xAD, r == 0x34F,
		r == 0x61C, r == 0x180E, r >= 0xE0000 && r <= 0xE007F:
		return true
	}
	return false
}

func stripInvisible(s string) string {
	if !strings.ContainsFunc(s, invisible) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if invisible(r) {
			return -1
		}
		return r
	}, s)
}

// homoglyphs maps lookalike letters from Cyrillic and Greek to the Latin letter
// they are drawn to match. The list is the common subset that makes an English
// phrase read as English; it is not a full Unicode confusables table, and the
// doc says so.
var homoglyphs = map[rune]rune{
	// Cyrillic, lowercase
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'у': 'y', 'х': 'x',
	'і': 'i', 'ј': 'j', 'ѕ': 's', 'ԁ': 'd', 'һ': 'h', 'ԛ': 'q', 'ԝ': 'w', 'ѵ': 'v',
	// Cyrillic, uppercase
	'А': 'A', 'В': 'B', 'Е': 'E', 'К': 'K', 'М': 'M', 'Н': 'H', 'О': 'O',
	'Р': 'P', 'С': 'C', 'Т': 'T', 'Х': 'X', 'І': 'I', 'Ј': 'J', 'Ѕ': 'S', 'Ү': 'Y',
	// Greek, lowercase
	'ο': 'o', 'α': 'a', 'ε': 'e', 'ι': 'i', 'ν': 'v', 'κ': 'k', 'τ': 't',
	'ρ': 'p', 'υ': 'u', 'χ': 'x',
	// Greek, uppercase
	'Α': 'A', 'Β': 'B', 'Ε': 'E', 'Ζ': 'Z', 'Η': 'H', 'Ι': 'I', 'Κ': 'K',
	'Μ': 'M', 'Ν': 'N', 'Ο': 'O', 'Ρ': 'P', 'Τ': 'T', 'Υ': 'Y', 'Χ': 'X',
}

func foldHomoglyphs(s string) string {
	if !strings.ContainsFunc(s, func(r rune) bool { _, ok := homoglyphs[r]; return ok }) {
		return s
	}
	return strings.Map(func(r rune) rune {
		if l, ok := homoglyphs[r]; ok {
			return l
		}
		return r
	}, s)
}

// collapseSpace turns every run of Unicode whitespace into one ASCII space and
// trims the ends. Non-breaking and other exotic spaces are included, because
// Go's RE2 \s matches ASCII only and a single NBSP would otherwise split a
// phrase the pattern expects to see whole.
func collapseSpace(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ")
}

// step is one decoder or reveal. ok is false when it found nothing to do, so
// that a variant is only produced when a transform changed the text.
type step struct {
	name  string
	apply func(string) (string, bool)
}

var steps = []step{
	{"base64", decodeBase64Runs},
	{"hex", decodeHexRuns},
	{"url", decodeURL},
	{"data-url", revealDataURLs},
	{"html-comment", revealHTMLComments},
	{"link-title", revealLinkTitles},
	{"alt-text", revealAltText},
	{"html-entity", unescapeEntities},
	{"rot13", rot13Step},
}

// printable reports whether s is valid UTF-8 that reads as text: every rune is
// a graphic character or whitespace, and it is not empty.
func printable(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) && !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func rot13Step(s string) (string, bool) {
	out := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			return 'A' + (r-'A'+13)%26
		}
		return r
	}, s)
	return out, out != s
}

// unescapeEntities decodes HTML character references. A renderer shows &#105;
// as "i"; a model reading the source sees the entity, so the decoded form is
// the reading a reader of the rendered page would have.
func unescapeEntities(s string) (string, bool) {
	if !strings.Contains(s, "&") {
		return "", false
	}
	out := html.UnescapeString(s)
	return out, out != s
}

func decodeURL(s string) (string, bool) {
	if strings.Count(s, "%") < 3 {
		return "", false
	}
	out, err := url.PathUnescape(s)
	if err != nil || out == s || len(out) > MaxDecoded || !printable(out) {
		return "", false
	}
	return out, true
}
