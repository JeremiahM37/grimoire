package main

import (
	"os"
	"strings"
	"unicode/utf8"

	"golang.org/x/term"
)

// Colour for the terminal commands, and nothing else.
//
// Colour is on only when stdout is a terminal, NO_COLOR is unset or empty (the
// no-color.org convention), and TERM is not "dumb". A pipe, a redirect or a
// script therefore always gets plain text, and so does anyone who asked for
// none. Nothing here writes escape codes into a file or a JSON body.

const (
	ansiBold  = "\x1b[1m"
	ansiDim   = "\x1b[2m"
	ansiReset = "\x1b[0m"
)

// snippetWidth is how many characters of a search snippet are printed.
const snippetWidth = 100

// colourAllowed reports whether f should receive colour codes.
func colourAllowed(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	// A real terminal, not merely a character device: /dev/null is one of
	// those too, and escape codes written there are harmless but a redirect to
	// any other device should stay plain.
	return term.IsTerminal(int(f.Fd()))
}

// styled wraps s in an ANSI style when on is true, and returns it unchanged
// otherwise.
func styled(on bool, code, s string) string {
	if !on || s == "" {
		return s
	}
	return code + s + ansiReset
}

// renderSnippet prepares a search snippet for one line of output: whitespace
// runs collapse to single spaces and the text is cut to snippetWidth runes.
//
// The server marks each matched term with [brackets]. Without colour those
// brackets stay, since they are the only way a plain-text reader sees the
// match. With colour the terms are bold and the brackets go. A cut that lands
// inside a bracketed term leaves no closing bracket. That term is printed plain
// with its stray opening bracket removed, rather than bolded wrongly.
func renderSnippet(s string, colour bool) string {
	flat := strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(flat) > snippetWidth {
		r := []rune(flat)
		flat = strings.TrimRight(string(r[:snippetWidth-1]), " ") + "…"
	}
	if !colour {
		return flat
	}
	var b strings.Builder
	for {
		open := strings.IndexByte(flat, '[')
		if open < 0 {
			break
		}
		shut := strings.IndexByte(flat[open+1:], ']')
		if shut < 0 {
			// The cut took the closing bracket. Drop the stray opener too.
			b.WriteString(flat[:open])
			b.WriteString(strings.ReplaceAll(flat[open+1:], "[", ""))
			return b.String()
		}
		b.WriteString(flat[:open])
		b.WriteString(styled(true, ansiBold, flat[open+1:open+1+shut]))
		flat = flat[open+1+shut+1:]
	}
	b.WriteString(flat)
	return b.String()
}
