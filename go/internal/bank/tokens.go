package bank

import (
	"unicode"
	"unicode/utf8"
)

// CountTokens estimates how many tokens a text costs a reader model.
//
// An estimate on purpose: the budgets it enforces (how many recalled facts
// fit in a prompt) are soft, and shipping a 200k-entry BPE vocabulary to count
// them exactly would be most of this binary's size. The estimate follows how
// modern byte-pair vocabularies actually split text — a common word is one
// token, a long or rare word several, digits go in groups of up to three, and
// each punctuation mark is its own — and lands within a few percent of an
// o200k count on English prose, erring high on code and IDs, which is the
// safe direction for a budget.
func CountTokens(s string) int {
	n := 0
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case unicode.IsSpace(r):
			i += size
		case unicode.IsLetter(r):
			j, letters := i, 0
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !unicode.IsLetter(r2) {
					break
				}
				letters++
				j += s2
			}
			if r > unicode.MaxLatin1 && !unicode.Is(unicode.Latin, r) {
				n += letters // non-Latin scripts: about a token per character
			} else {
				n += 1 + (letters-1)/6
			}
			i = j
		case unicode.IsDigit(r):
			j, digits := i, 0
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !unicode.IsDigit(r2) {
					break
				}
				digits++
				j += s2
			}
			n += (digits + 2) / 3
			i = j
		default:
			n++
			i += size
		}
	}
	return n
}
