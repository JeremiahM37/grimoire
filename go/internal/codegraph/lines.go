package codegraph

import (
	"errors"
	"strings"
)

var errUnsupported = errors.New("codegraph: unsupported file type")

// splitLines breaks source into lines without their terminators.
func splitLines(src []byte) []string {
	lines := strings.Split(string(src), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// indentOf is the width of a line's leading whitespace. A tab counts as one
// column: the scanners only compare indents against each other, never against
// a rendered width.
func indentOf(s string) int {
	return len(s) - len(strings.TrimLeft(s, " \t"))
}

// blockEnd finds the last line of a block that opens at lines[start] with
// the given indent: the last non-blank line before the first later non-blank
// line that is no more indented. For brace languages, a closing brace at that
// same indent belongs to the block and is included.
//
// It is an approximation. It does not read strings or comments, so a line of
// a multi-line string that starts flush left ends the block early. The
// result is 1-based.
func blockEnd(lines []string, start, indent int, braces bool) int {
	last := start
	for j := start + 1; j < len(lines); j++ {
		t := strings.TrimSpace(lines[j])
		if t == "" {
			continue
		}
		if indentOf(lines[j]) <= indent {
			if braces && (strings.HasPrefix(t, "}") || strings.HasPrefix(t, ")") || strings.HasPrefix(t, "]")) {
				last = j
			}
			break
		}
		last = j
	}
	return last + 1
}

// scope is one open block in the indentation-tracking scanners. qual is the
// qualified name of a function, which is the caller for calls inside it.
type scope struct {
	indent int
	name   string
	kind   string
	qual   string
}

// popTo drops every open block that a line at this indent has closed.
func popTo(stack []scope, indent int) []scope {
	for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
		stack = stack[:len(stack)-1]
	}
	return stack
}

// litState carries a literal or comment that is still open at the end of a
// line: the text that closes it, or "" when the next line starts in code.
type litState struct{ close string }

// codeOnly returns line with comments and the contents of string literals
// replaced by spaces, so that the call and brace scanners never see a `(` or
// `{` inside text. Delimiters are blanked too, so "f(\"x\")" still reads as a
// call to f. Columns are preserved. A multi-line literal (a Python triple-quoted
// string, a JS template literal or block comment) is carried in st to the next
// line.
//
// It is a scanner, not a parser. A regular expression literal in JS that holds
// a quote can confuse it for the rest of that line.
func codeOnly(line string, st *litState, lang string) string {
	b := []byte(line)
	blank := func(from, to int) {
		for k := from; k < to && k < len(b); k++ {
			b[k] = ' '
		}
	}
	i := 0
	if st.close != "" {
		end := strings.Index(line, st.close)
		if end < 0 {
			blank(0, len(b))
			return string(b)
		}
		i = end + len(st.close)
		blank(0, i)
		st.close = ""
	}
	py := lang == LangPython
	for i < len(b) {
		c := b[i]
		if py && c == '#' || !py && c == '/' && i+1 < len(b) && b[i+1] == '/' {
			blank(i, len(b))
			break
		}
		if !py && c == '/' && i+1 < len(b) && b[i+1] == '*' {
			end := strings.Index(line[i+2:], "*/")
			if end < 0 {
				blank(i, len(b))
				st.close = "*/"
				break
			}
			to := i + 2 + end + 2
			blank(i, to)
			i = to
			continue
		}
		if c != '"' && c != '\'' && (py || c != '`') {
			i++
			continue
		}
		delim := string(c)
		if py && strings.HasPrefix(line[i:], delim+delim+delim) {
			delim = delim + delim + delim
		}
		start := i
		j := i + len(delim)
		closed := false
		for j < len(b) {
			if b[j] == '\\' && c != '`' {
				j += 2
				continue
			}
			if strings.HasPrefix(line[j:], delim) {
				j += len(delim)
				closed = true
				break
			}
			j++
		}
		if !closed {
			if len(delim) == 1 && c != '`' {
				// An unterminated single-line string ends with its line.
				blank(start, len(b))
				break
			}
			st.close = delim
			blank(start, len(b))
			break
		}
		blank(start, j)
		i = j
	}
	return string(b)
}
