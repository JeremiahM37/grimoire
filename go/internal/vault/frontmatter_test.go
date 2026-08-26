package vault

import (
	"encoding/json"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

type fmFixtureFile struct {
	Cases []struct {
		Name        string               `json:"name"`
		RawInner    string               `json:"raw_inner"`
		Frontmatter compat.OrderedObject `json:"frontmatter"`
		Patched     string               `json:"patched"`
	} `json:"cases"`
	SerializeCases []struct {
		Frontmatter compat.OrderedObject `json:"frontmatter"`
		Body        string               `json:"body"`
		Serialized  string               `json:"serialized"`
	} `json:"serialize_cases"`
}

// frontmatterFrom rebuilds an ordered Frontmatter from an ordered fixture object.
func frontmatterFrom(t *testing.T, o compat.OrderedObject) *markdown.Frontmatter {
	t.Helper()
	fm := markdown.NewFrontmatter()
	for _, k := range o.Keys {
		var v any
		if err := o.Decode(k, &v); err != nil {
			t.Fatalf("decoding %q: %v", k, err)
		}
		fm.Set(k, jsonToValue(v))
	}
	return fm
}

// jsonToValue maps decoded JSON into frontmatter values. Numbers arrive as
// float64 and must render the way Python's str() would, or `port: 8443` comes
// back as `port: 8443.0`.
func jsonToValue(v any) markdown.Value {
	switch t := v.(type) {
	case []any:
		out := make([]markdown.Value, len(t))
		for i, item := range t {
			out[i] = jsonToValue(item)
		}
		return out
	case float64:
		if t == float64(int64(t)) {
			return int64(t)
		}
		return t
	case json.Number:
		return t.String()
	}
	return v
}

// The guarantee that keeps another app's vault intact: nested maps, block
// scalars and foreign keys survive a write byte for byte.
func TestPatchFrontmatterMatchesPython(t *testing.T) {
	var fx fmFixtureFile
	compat.Load(t, "frontmatter.json", &fx)
	if len(fx.Cases) == 0 {
		t.Fatal("no patch cases in fixture")
	}
	for _, c := range fx.Cases {
		got := PatchFrontmatter(c.RawInner, frontmatterFrom(t, c.Frontmatter))
		if got != c.Patched {
			t.Errorf("%s:\n got: %q\nwant: %q", c.Name, got, c.Patched)
		}
	}
}

func TestSerializeMatchesPython(t *testing.T) {
	var fx fmFixtureFile
	compat.Load(t, "frontmatter.json", &fx)
	for i, c := range fx.SerializeCases {
		got := Serialize(frontmatterFrom(t, c.Frontmatter), c.Body)
		if got != c.Serialized {
			t.Errorf("serialize case %d:\n got: %q\nwant: %q", i, got, c.Serialized)
		}
	}
}

// A written note must parse back to what was written — otherwise every
// save-then-read cycle degrades the file a little more.
func TestSerializeParseRoundTrip(t *testing.T) {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Round Trip")
	fm.Set("tags", []markdown.Value{"alpha", "beta"})
	fm.Set("private", true)

	text := Serialize(fm, "# Round Trip\n\nbody text\n")
	n := NoteFromText("rt.md", text, 0)

	if n.Title != "Round Trip" {
		t.Errorf("title = %q", n.Title)
	}
	if !n.Private {
		t.Error("private flag lost in round trip")
	}
	if len(n.Tags) != 2 || n.Tags[0] != "alpha" || n.Tags[1] != "beta" {
		t.Errorf("tags = %v, want [alpha beta]", n.Tags)
	}
}

// A list that arrives as JSON must render as a list the parser can read back.
//
// []any fell through to Go's %v — "tags: [a b]", space separated — while
// ParseFrontmatter splits a bracketed list on COMMAS. So a note written through
// the API with two tags read back as the single tag "a b", every lookup for
// either found nothing, and the file was not valid YAML so Obsidian could not
// read it either. Nothing errored at any point.
func TestJSONListsRoundTripThroughFrontmatter(t *testing.T) {
	line := FMEntry("tags", []any{"conversation", "chatgpt"})
	if line != "tags: [conversation, chatgpt]" {
		t.Fatalf("FMEntry rendered %q, want a comma-separated list", line)
	}
	fm, _ := markdown.ParseFrontmatter("---\n" + line + "\n---\n\nbody\n")
	v, ok := fm.Get("tags")
	if !ok {
		t.Fatal("tags key lost on the way back")
	}
	items, ok := v.([]markdown.Value)
	if !ok {
		t.Fatalf("tags parsed back as %T, not a list", v)
	}
	if len(items) != 2 {
		t.Fatalf("got %d tags, want 2 — a space-separated list reads back as one", len(items))
	}
	if items[0] != markdown.Value("conversation") || items[1] != markdown.Value("chatgpt") {
		t.Errorf("tags round-tripped as %v", items)
	}
}

// Mixed and non-string JSON lists must not regress to %v either.
func TestJSONListsOfNonStringsAlsoRender(t *testing.T) {
	if got := FMEntry("nums", []any{1, 2}); got != "nums: [1, 2]" {
		t.Errorf("FMEntry(nums) = %q", got)
	}
	if got := FMEntry("empty", []any{}); got != "empty: []" {
		t.Errorf("FMEntry(empty) = %q", got)
	}
}
