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

// scope is one open block in the indentation-tracking scanners.
type scope struct {
	indent int
	name   string
	kind   string
}

// popTo drops every open block that a line at this indent has closed.
func popTo(stack []scope, indent int) []scope {
	for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
		stack = stack[:len(stack)-1]
	}
	return stack
}
