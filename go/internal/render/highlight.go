package render

import (
	"regexp"
	"strings"
)

// Dependency-free, language-agnostic syntax highlighting — the same token set
// the Python renderer uses, so fenced code blocks come out identical.

var keywords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`const let var function func fn return if else elif
for while class struct interface type enum import export from package use pub async
await new def lambda try except catch finally throw with match case switch break
continue in of is not and or public private protected static void int float double
string str bool true false True False None null nil self this super impl trait yield
do then end module namespace typedef extends implements`) {
		keywords[w] = true
	}
}

// Built from a plain quoted string rather than a raw literal because the string
// alternative has to contain a backtick. Groups, in order: comments, strings,
// numbers, identifiers — the switch in highlightToken depends on that order.
// The identifier class is Unicode-aware to match Python's \w.
var tokenRE = regexp.MustCompile(
	"(//[^\n]*|#[^\n]*|/\\*[\\s\\S]*?\\*/)" +
		"|(\"(?:\\\\.|[^\"\\\\])*\"|'(?:\\\\.|[^'\\\\])*'|`(?:\\\\.|[^`\\\\])*`)" +
		"|(\\b\\d[\\d_.]*(?:[eE][+-]?\\d+)?\\b|\\b0[xX][0-9a-fA-F]+\\b)" +
		"|([A-Za-z_$][\\p{L}\\p{M}\\p{N}_$]*)")

// HighlightCode wraps comments, strings, numbers and keywords in spans, escaping
// everything else. Escaping happens per-segment so the emitted markup is never
// itself escaped.
func HighlightCode(code, lang string) string {
	var out strings.Builder
	last := 0
	for _, m := range tokenRE.FindAllStringSubmatchIndex(code, -1) {
		out.WriteString(escapeHTML(code[last:m[0]]))
		out.WriteString(highlightToken(code, m))
		last = m[1]
	}
	out.WriteString(escapeHTML(code[last:]))
	return out.String()
}

func highlightToken(code string, m []int) string {
	group := func(i int) string {
		lo, hi := m[2*i], m[2*i+1]
		if lo < 0 {
			return ""
		}
		return code[lo:hi]
	}
	if s := group(1); s != "" {
		return `<span class="hl-com">` + escapeHTML(s) + `</span>`
	}
	if s := group(2); s != "" {
		return `<span class="hl-str">` + escapeHTML(s) + `</span>`
	}
	if s := group(3); s != "" {
		return `<span class="hl-num">` + escapeHTML(s) + `</span>`
	}
	word := group(4)
	if keywords[word] {
		return `<span class="hl-kw">` + escapeHTML(word) + `</span>`
	}
	return escapeHTML(word)
}
