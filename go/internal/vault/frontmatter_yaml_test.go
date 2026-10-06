package vault

import (
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// A value that plain YAML cannot hold has to be written quoted, and has to
// read back unchanged. "Memory: ops" is the one that mattered: every memory
// note carried it unquoted, which Obsidian rejects as "Invalid properties".
func TestFrontmatterStringsSurviveYAML(t *testing.T) {
	values := []string{
		"Memory: ops",
		"ends with colon:",
		"a # not a comment",
		"- looks like a list",
		"[looks like a flow list]",
		`has "double" quotes: yes`,
		"it's fine",
		"'single quoted on purpose'",
		`back\slash: here`,
		"two\nlines: here",
		"@handle",
		"plain value",
		"2026-10-06T09:33:04",
		"https://example.com/a:b",
	}
	for _, want := range values {
		fm := markdown.NewFrontmatter()
		fm.Set("title", want)
		file := Serialize(fm, "# body\n")
		parsed, _ := markdown.ParseFrontmatter(file)
		got, _ := parsed.Get("title")
		if got != want {
			t.Errorf("%q came back as %q from:\n%s", want, got, file)
		}
	}
}

// Edge whitespace is trimmed, as plain YAML always trimmed it here.
func TestEdgeWhitespaceIsTrimmedNotQuoted(t *testing.T) {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "  long title ")
	parsed, _ := markdown.ParseFrontmatter(Serialize(fm, "body\n"))
	if got, _ := parsed.Get("title"); got != "long title" {
		t.Fatalf("got %q", got)
	}
	fm.Set("title", " Memory: ops ")
	parsed, _ = markdown.ParseFrontmatter(Serialize(fm, "body\n"))
	if got, _ := parsed.Get("title"); got != "Memory: ops" {
		t.Fatalf("a quoted value kept its edge whitespace: %q", got)
	}
}

func TestPlainValuesStayPlain(t *testing.T) {
	for _, v := range []string{"plain value", "2026-10-06T09:33:04", "https://example.com/a:b", "codex"} {
		if line := FMEntry("k", v); line != "k: "+v {
			t.Errorf("%q was quoted unnecessarily: %s", v, line)
		}
	}
	if line := FMEntry("title", "Memory: ops"); line != `title: "Memory: ops"` {
		t.Errorf("got %s", line)
	}
}

// Hand-quoted values written by other tools keep reading as they did.
func TestQuotedValuesFromOtherTools(t *testing.T) {
	cases := map[string]string{
		`title: "Memory: ops"`:      "Memory: ops",
		`title: 'it''s mine'`:       "it's mine",
		`title: "a \"b\" c"`:        `a "b" c`,
		`title: "unbalanced`:        "unbalanced",
		`title: plain "inner" text`: `plain "inner" text`,
	}
	for line, want := range cases {
		fm, _ := markdown.ParseFrontmatter("---\n" + line + "\n---\nbody\n")
		got, _ := fm.Get("title")
		if got != want {
			t.Errorf("%s read as %q, want %q", line, got, want)
		}
	}
	if !strings.Contains(FMEntry("t", `x: "y"`), `\"y\"`) {
		t.Error("embedded quotes must be escaped")
	}
}
