package api

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Injected text is read by a model on every prompt it fires on, unasked, so it
// is cut to what carries meaning. A note chunk is up to 600 characters of
// whatever the chunker cut: markdown tables, headings, emphasis, emoji, half a
// sentence at each end. compactNote keeps the sentences of the chunk that
// match the request and drops the formatting; compactFact keeps a stored fact
// whole but plain. The note itself is unchanged and one read_note away (its
// path is on every line).

const (
	// noteExcerptMax is the most of a note chunk an injected line carries.
	noteExcerptMax = 260
	// factMax is the most of a stored fact. Facts are written to be short;
	// this only stops a pasted paragraph.
	factMax = 360
)

var (
	compactFrontmatter = regexp.MustCompile(`(?s)\A\s*---\n.*?\n---\s*\n`)
	compactComment     = regexp.MustCompile(`(?s)<!--.*?-->`)
	compactImage       = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
	compactLink        = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	compactWiki        = regexp.MustCompile(`\[\[([^\]|#]+)(?:#[^\]|]*)?(?:\|([^\]]+))?\]\]`)
	compactStrong      = regexp.MustCompile(`(\*\*|__)(\S(?:.*?\S)?)(\*\*|__)`)
	compactEm          = regexp.MustCompile(`(^|[\s(])\*(\S(?:[^*]*\S)?)\*`)
	compactHeading     = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	compactRule        = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,}|(?:=\s*){3,})$`)
	compactTableSep    = regexp.MustCompile(`^\s*\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?\s*$`)
	compactBullet      = regexp.MustCompile(`^\s*(?:[-*+•]|\d+[.)]|\[[ xX]\])\s+`)
	compactQuote       = regexp.MustCompile(`^\s*>+\s?`)
	compactTag         = regexp.MustCompile(`(^|\s)#[A-Za-z][\w/-]*`)
)

// plainSegments turns markdown into plain text segments: one per paragraph,
// bullet, heading or table row, split again at sentence ends. Lines that only
// wrap a paragraph are joined first, so a hard-wrapped note does not read as
// a sentence per line.
func plainSegments(text string) []string {
	text = compactFrontmatter.ReplaceAllString(text, "")
	text = compactComment.ReplaceAllString(text, " ")
	var blocks []string
	var para []string
	flush := func() {
		if len(para) > 0 {
			blocks = append(blocks, strings.Join(para, " "))
			para = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimSpace(line)
		switch {
		case t == "" || compactRule.MatchString(line) || compactTableSep.MatchString(line):
			flush()
		case strings.HasPrefix(t, "|"):
			// A table row reads as its cells.
			flush()
			var cells []string
			for _, c := range strings.Split(strings.Trim(t, "|"), "|") {
				if c = strings.TrimSpace(c); c != "" {
					cells = append(cells, c)
				}
			}
			blocks = append(blocks, strings.Join(cells, ": "))
		case compactHeading.MatchString(line):
			flush()
			blocks = append(blocks, compactHeading.ReplaceAllString(line, ""))
		case compactBullet.MatchString(line) || compactQuote.MatchString(line):
			// A bullet or a quote starts its own block; lines after it that
			// are not bullets continue it.
			flush()
			para = append(para, compactBullet.ReplaceAllString(compactQuote.ReplaceAllString(line, ""), ""))
		default:
			para = append(para, t)
		}
	}
	flush()
	var out []string
	for _, block := range blocks {
		for _, s := range sentences(plainInline(block)) {
			// A title repeated as the first heading (or a heading repeated
			// as the first line) says the same thing twice.
			if s = strings.TrimSpace(s); s != "" && (len(out) == 0 || !sameSegment(out[len(out)-1], s)) {
				out = append(out, s)
			}
		}
	}
	return out
}

// sameSegment reports two segments that differ only in case or a closing
// stop.
func sameSegment(a, b string) bool {
	trim := func(s string) string { return strings.ToLower(strings.TrimRight(s, ".:;! ")) }
	return trim(a) == trim(b)
}

// plainInline drops inline markdown and pictographs from one line.
func plainInline(line string) string {
	line = compactImage.ReplaceAllString(line, "")
	line = compactLink.ReplaceAllString(line, "$1")
	line = compactWiki.ReplaceAllStringFunc(line, func(m string) string {
		parts := compactWiki.FindStringSubmatch(m)
		if parts[2] != "" {
			return parts[2]
		}
		return parts[1]
	})
	line = compactStrong.ReplaceAllString(line, "$2")
	line = compactEm.ReplaceAllString(line, "$1$2")
	line = compactTag.ReplaceAllString(line, "$1")
	// Inline code keeps its text; the ticks only cost tokens.
	line = strings.ReplaceAll(line, "`", "")
	line = strings.Map(func(r rune) rune {
		switch {
		case r == '‍' || r == '️' || r == '︎':
			return -1
		case unicode.Is(unicode.So, r) || unicode.Is(unicode.Sk, r) && r > 0x2000:
			return -1
		}
		return r
	}, line)
	return strings.Join(strings.Fields(line), " ")
}

// sentences splits at ". ", "! ", "? " when the next word starts a sentence.
func sentences(line string) []string {
	var out []string
	start := 0
	for i := 0; i+2 < len(line); i++ {
		c := line[i]
		if (c == '.' || c == '!' || c == '?') && line[i+1] == ' ' {
			next, _ := utf8.DecodeRuneInString(line[i+2:])
			if unicode.IsUpper(next) || unicode.IsDigit(next) {
				out = append(out, line[start:i+1])
				start = i + 2
			}
		}
	}
	return append(out, line[start:])
}

// placeholderOnly reports a segment that is little more than the "…" a
// condensed command left behind.
func placeholderOnly(s string) bool {
	rest := strings.TrimSpace(strings.NewReplacer("…", "", ":", "", "-", "", "→", "").Replace(s))
	return len(rest) < 12
}

// compactNote keeps the segments of a note chunk that share the most terms
// with the request, in their order, within limit runes; with no match, the
// chunk's opening.
func compactNote(text string, terms []string, limit int) string {
	segs := plainSegments(text)
	if len(segs) == 0 {
		return ""
	}
	want := map[string]bool{}
	for _, t := range terms {
		want[t] = true
	}
	type scored struct {
		i, score int
	}
	ranked := make([]scored, 0, len(segs))
	for i, s := range segs {
		if placeholderOnly(s) {
			continue
		}
		n := 0
		seen := map[string]bool{}
		for _, t := range memory.StemTokens(s) {
			if want[t] && !seen[t] {
				seen[t] = true
				n++
			}
		}
		// A sentence with commands cut out of it reads in pieces; prefer
		// one that says the same thing whole.
		if n > 0 {
			n = n*2 - strings.Count(s, "…")
		}
		ranked = append(ranked, scored{i, max(n, 0)})
	}
	if len(ranked) == 0 {
		return ""
	}
	best := ranked[0].score
	for _, r := range ranked {
		best = max(best, r.score)
	}
	keep := map[int]bool{}
	used := 0
	if best > 0 {
		// The strongest segments first, then fill in order.
		order := append([]scored(nil), ranked...)
		for a := 1; a < len(order); a++ {
			for b := a; b > 0 && order[b].score > order[b-1].score; b-- {
				order[b], order[b-1] = order[b-1], order[b]
			}
		}
		for _, r := range order {
			if r.score == 0 {
				break
			}
			n := utf8.RuneCountInString(segs[r.i]) + 1
			if used > 0 && used+n > limit {
				continue
			}
			keep[r.i] = true
			used += n
		}
	} else {
		for _, r := range ranked {
			n := utf8.RuneCountInString(segs[r.i]) + 1
			if used > 0 && used+n > limit {
				break
			}
			keep[r.i] = true
			used += n
		}
	}
	var b strings.Builder
	last := -2
	for i, s := range segs {
		if !keep[i] {
			continue
		}
		if b.Len() > 0 {
			if i == last+1 {
				b.WriteString(" ")
			} else {
				b.WriteString(" … ")
			}
		}
		b.WriteString(endSentence(s))
		last = i
	}
	return clipWords(b.String(), limit)
}

// compactFact is a stored fact as plain text.
func compactFact(text string) string {
	segs := plainSegments(text)
	for i := range segs {
		segs[i] = endSentence(segs[i])
	}
	return clipWords(strings.Join(segs, " "), factMax)
}

// endSentence ends a segment that came from its own line, so joined lines
// still read as separate statements.
func endSentence(s string) string {
	if s == "" {
		return s
	}
	switch last, _ := utf8.DecodeLastRuneInString(s); last {
	case '.', '!', '?', ':', ';', '…', ')', '"', '\'':
		return s
	}
	return s + "."
}

// clipWords cuts s to max runes at a word boundary.
func clipWords(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	cut := string(r[:max-1])
	if i := strings.LastIndexByte(cut, ' '); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,;:") + "…"
}
