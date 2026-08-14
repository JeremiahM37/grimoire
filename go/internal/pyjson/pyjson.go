// Package pyjson encodes JSON exactly as Python's json.dumps does with its
// defaults.
//
// This matters wherever JSON is STORED rather than merely transmitted — the
// CRDT documents on disk and the frontmatter_json index column — because the Go
// and Python implementations have to produce identical bytes there. Two
// differences would otherwise creep in silently:
//
//   - separators: Python defaults to ", " and ": "; Go's encoding/json emits
//     "," and ":". A LIKE '%"pinned": true%' query written against one shape
//     matches nothing against the other.
//   - ensure_ascii: Python escapes every non-ASCII rune as \uXXXX (with
//     surrogate pairs above the BMP); Go emits raw UTF-8. Any note with an emoji
//     in its title stores different bytes.
//
// Go additionally HTML-escapes <, > and & by default, which Python does not.
package pyjson

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

// String encodes a Go string as a Python-compatible JSON string literal.
func String(s string) string {
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
				hi, lo := utf16.EncodeRune(r)
				fmt.Fprintf(&b, `\u%04x\u%04x`, hi, lo)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// Value encodes a value the way json.dumps would, with ", " / ": " separators.
// Map keys are sorted, matching Python dicts whose insertion order the caller
// does not control; ordered structures should use Object instead.
func Value(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		if t {
			return "true"
		}
		return "false"
	case string:
		return String(t)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		// Python renders an integral float as "1.0"; Go's %v gives "1"
		if t == float64(int64(t)) {
			return strconv.FormatInt(int64(t), 10) + ".0"
		}
		return strconv.FormatFloat(t, 'g', -1, 64)
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = Value(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []string:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = String(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		pairs := make([]Pair, len(keys))
		for i, k := range keys {
			pairs[i] = Pair{k, t[k]}
		}
		return Object(pairs)
	}
	return String(fmt.Sprint(v))
}

// Pair is one key/value entry, used where key ORDER is significant.
type Pair struct {
	Key   string
	Value any
}

// Object encodes an object preserving the given key order.
func Object(pairs []Pair) string {
	parts := make([]string, len(pairs))
	for i, p := range pairs {
		parts[i] = String(p.Key) + ": " + Value(p.Value)
	}
	return "{" + strings.Join(parts, ", ") + "}"
}
