package pyjson

import (
	"strings"
	"testing"
)

// This package exists for one reason: the bytes it writes must be identical to
// what Python's json.dumps produces, because the same values are read back by
// LIKE patterns and compared across the two implementations. The expectations
// below are therefore not hand-written — each `want` is the literal output of
// json.dumps for that input, which makes this a differential test against the
// reference implementation rather than a restatement of the Go code.

func TestStringMatchesPythonJSONDumps(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"plain", "hello", "\"hello\""},
		{"empty", "", "\"\""},
		{"quote", "he said \"hi\"", "\"he said \\\"hi\\\"\""},
		{"backslash", "a\\b", "\"a\\\\b\""},
		{"newline and tab", "line1\nline2\tend", "\"line1\\nline2\\tend\""},
		{"control chars", "bell\u0007null\u0000", "\"bell\\u0007null\\u0000\""},
		{"delete", "\u007f", "\"\\u007f\""},
		{"html chars", "<script>a & b</script>", "\"<script>a & b</script>\""},
		{"forward slash", "a/b", "\"a/b\""},
		{"accented", "caf\u00e9", "\"caf\\u00e9\""},
		{"cjk", "\u65e5\u672c\u8a9e", "\"\\u65e5\\u672c\\u8a9e\""},
		{"combining mark", "e\u0301", "\"e\\u0301\""},
		{"bmp edge", "\uffff", "\"\\uffff\""},
		{"emoji", "grimoire \U0001f4da", "\"grimoire \\ud83d\\udcda\""},
		{"astral start", "\U0001f600 face", "\"\\ud83d\\ude00 face\""},
	}
	for _, c := range cases {
		if got := String(c.input); got != c.want {
			t.Errorf("%s: String(%q)\n got: %s\nwant: %s", c.name, c.input, got, c.want)
		}
	}
}

func TestValueMatchesPythonJSONDumps(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  string
	}{
		{"nil", nil, "null"},
		{"true", true, "true"},
		{"false", false, "false"},
		{"int", 42, "42"},
		{"negative", -7, "-7"},
		{"zero", 0, "0"},
		{"float", 1.5, "1.5"},
		{"empty list", []any{}, "[]"},
		{"list", []any{"a", 1, true, nil}, "[\"a\", 1, true, null]"},
		{"empty map", map[string]any{}, "{}"},
		{"map is key-sorted", map[string]any{"b": 1, "a": 2}, "{\"a\": 2, \"b\": 1}"},
		{"nested", map[string]any{"tags": []any{"x", "y"}, "pinned": true}, "{\"pinned\": true, \"tags\": [\"x\", \"y\"]}"},
		{"unicode in keys and values", map[string]any{"t\u00edtulo": "caf\u00e9 \u2615"}, "{\"t\\u00edtulo\": \"caf\\u00e9 \\u2615\"}"},
	}
	for _, c := range cases {
		if got := Value(c.input); got != c.want {
			t.Errorf("%s: Value(%#v)\n got: %s\nwant: %s", c.name, c.input, got, c.want)
		}
	}
}

// Object preserves the caller's key order, which is what frontmatter needs: a
// note's own key order is meaningful and has to survive a round trip.
func TestObjectPreservesKeyOrder(t *testing.T) {
	got := Object([]Pair{
		{Key: "title", Value: "Note"},
		{Key: "created", Value: "2026-08-17"},
		{Key: "pinned", Value: true},
		{Key: "aliases", Value: []any{"n", "note"}},
	})
	want := `{"title": "Note", "created": "2026-08-17", "pinned": true, "aliases": ["n", "note"]}`
	if got != want {
		t.Errorf("Object()\n got: %s\nwant: %s", got, want)
	}
	if got := Object(nil); got != `{}` {
		t.Errorf("Object(nil) = %s, want {}", got)
	}
}

// The pinned-notes lookup is a LIKE against this exact rendering. Asserted
// literally because a drifted separator makes the briefing endpoint return an
// empty pinned list with no error at all.
func TestPinnedPatternMatchesStoredFrontmatter(t *testing.T) {
	stored := Object([]Pair{{Key: "pinned", Value: true}})
	const pattern = `"pinned": true`
	if !strings.Contains(stored, pattern) {
		t.Errorf("stored frontmatter %s does not contain the LIKE pattern %q "+
			"that the briefing query searches for", stored, pattern)
	}
}
