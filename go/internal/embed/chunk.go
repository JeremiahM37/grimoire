package embed

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// ChunkTarget is the character budget a chunk is packed toward.
const ChunkTarget = 800

var (
	blankLineRE = regexp.MustCompile(`\n\s*\n`)
	sentenceRE  = regexp.MustCompile(`(?:[.!?])\s+`)
)

// ChunkText splits on blank lines (long paragraphs split further on lines, then
// sentences), then greedily packs pieces toward `target` characters.
//
// Chunk boundaries decide what retrieval can return, so this has to match the
// Python implementation exactly — a different split silently changes which
// passages are retrievable at all.
//
// Sizes are counted in CHARACTERS, not bytes. Python's len() counts code
// points, so measuring Go's byte length packs fewer characters per chunk for
// any note containing an emoji or an accent — a divergence that shows up only
// on non-ASCII notes and only as different search results.
func ChunkText(text string) []string {
	return ChunkTextTarget(text, ChunkTarget)
}

// ChunkTextTarget is ChunkText with an explicit target size.
func ChunkTextTarget(text string, target int) []string {
	var paras []string
	for _, p := range blankLineRE.Split(text, -1) {
		if strings.TrimSpace(p) == "" {
			continue
		}
		paras = append(paras, splitLongPara(strings.TrimSpace(p), target)...)
	}

	var chunks []string
	cur := ""
	curLen := 0
	for _, p := range paras {
		pLen := utf8.RuneCountInString(p)
		if curLen+pLen+2 > target && cur != "" {
			chunks = append(chunks, strings.TrimSpace(cur))
			cur, curLen = p, pLen
			continue
		}
		if cur == "" {
			cur, curLen = p, pLen
		} else {
			cur = cur + "\n\n" + p
			curLen += pLen + 2
		}
	}
	if strings.TrimSpace(cur) != "" {
		chunks = append(chunks, strings.TrimSpace(cur))
	}
	if len(chunks) > 0 {
		return chunks
	}
	if t := strings.TrimSpace(text); t != "" {
		return []string{t}
	}
	return []string{}
}

// splitLongPara keeps a paragraph without blank lines (a transcript, a log,
// hard-wrapped prose) from becoming one giant chunk: split on line boundaries,
// falling back to sentence boundaries for a single enormous line.
func splitLongPara(p string, target int) []string {
	if float64(utf8.RuneCountInString(p)) <= float64(target)*1.5 {
		return []string{p}
	}
	units := splitLines(p)
	if len(units) == 1 {
		units = splitSentences(p)
	}
	var out []string
	cur := ""
	curLen := 0
	for _, u := range units {
		uLen := utf8.RuneCountInString(u)
		if curLen+uLen+1 > target && cur != "" {
			out = append(out, cur)
			cur, curLen = u, uLen
			continue
		}
		if cur == "" {
			cur, curLen = u, uLen
		} else {
			cur = cur + "\n" + u
			curLen += uLen + 1
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

// splitLines mirrors Python's str.splitlines for the separators that occur in
// note text (it does not split on the exotic ones Python also handles, which
// cannot reach here because clean-up already normalized them).
func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	// Python drops a single trailing empty element, since splitlines does not
	// produce one for a trailing newline.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	return lines
}

// splitSentences implements re.split(r"(?<=[.!?])\s+", p) without lookbehind:
// split after sentence punctuation, dropping the whitespace run.
func splitSentences(p string) []string {
	var out []string
	last := 0
	for _, loc := range sentenceRE.FindAllStringIndex(p, -1) {
		// loc[0] is the punctuation; the lookbehind form keeps it with the
		// preceding sentence, so the cut goes after it.
		cut := loc[0] + 1
		out = append(out, p[last:cut])
		last = loc[1]
	}
	out = append(out, p[last:])
	return out
}
