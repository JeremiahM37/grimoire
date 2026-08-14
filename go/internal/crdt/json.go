package crdt

import (
	"fmt"
	"strings"
	"unicode/utf16"
)

// pyJSONString encodes a string exactly as Python's json.dumps does with its
// defaults (ensure_ascii=True).
//
// Go's encoding/json differs from Python in two ways that both change bytes:
// it emits non-ASCII raw instead of \uXXXX, and it escapes <, > and & for HTML
// safety. Since the CRDT file is an on-disk format shared between the two
// implementations, matching Python here keeps switching implementations from
// silently rewriting every stored document with different escaping.
func pyJSONString(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		default:
			switch {
			case r < 0x20:
				fmt.Fprintf(&b, `\u%04x`, r)
			case r < 0x7f:
				b.WriteRune(r)
			case r <= 0xffff:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				// astral plane: Python emits a UTF-16 surrogate pair
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
