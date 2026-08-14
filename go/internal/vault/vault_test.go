package vault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

type notesFixture struct {
	MTime float64 `json:"mtime"`
	Cases []struct {
		Path        string          `json:"path"`
		Text        string          `json:"text"`
		Title       string          `json:"title"`
		Body        string          `json:"body"`
		Tags        []string        `json:"tags"`
		Links       []markdown.Link `json:"links"`
		Private     bool            `json:"private"`
		Encrypted   bool            `json:"encrypted"`
		Frontmatter map[string]any  `json:"frontmatter"`
		Hash        string          `json:"hash"`
		Facts       [][]string      `json:"facts"`
	} `json:"cases"`
}

type pathsFixture struct {
	Cases []struct {
		Rel         string `json:"rel"`
		OK          bool   `json:"ok"`
		ResolvedRel string `json:"resolved_rel"`
		Error       string `json:"error"`
		IsReserved  bool   `json:"is_reserved"`
	} `json:"cases"`
	SlugifyCases []struct {
		Title string `json:"title"`
		Slug  string `json:"slug"`
	} `json:"slugify_cases"`
}

func TestNoteParsingMatchesPython(t *testing.T) {
	var fx notesFixture
	compat.Load(t, "notes.json", &fx)
	for _, c := range fx.Cases {
		n := NoteFromText(c.Path, c.Text, fx.MTime)
		if n.Title != c.Title {
			t.Errorf("%s: title = %q, want %q", c.Path, n.Title, c.Title)
		}
		if n.Body != c.Body {
			t.Errorf("%s: body = %q, want %q", c.Path, n.Body, c.Body)
		}
		if !reflect.DeepEqual(nonNil(n.Tags), nonNil(c.Tags)) {
			t.Errorf("%s: tags = %v, want %v", c.Path, n.Tags, c.Tags)
		}
		if !reflect.DeepEqual(nonNilLinks(n.Links), nonNilLinks(c.Links)) {
			t.Errorf("%s: links = %+v, want %+v", c.Path, n.Links, c.Links)
		}
		if n.Private != c.Private {
			t.Errorf("%s: private = %v, want %v", c.Path, n.Private, c.Private)
		}
		if n.Encrypted != c.Encrypted {
			t.Errorf("%s: encrypted = %v, want %v", c.Path, n.Encrypted, c.Encrypted)
		}
		if n.Hash != c.Hash {
			t.Errorf("%s: hash = %s, want %s", c.Path, n.Hash, c.Hash)
		}
		assertFrontmatterEqual(t, c.Path, n.Frontmatter, c.Frontmatter)
	}
}

// assertFrontmatterEqual compares parsed frontmatter against the fixture's JSON
// object, which is unordered — so this checks membership and values only.
func assertFrontmatterEqual(t *testing.T, name string, got *markdown.Frontmatter, want map[string]any) {
	t.Helper()
	if got.Len() != len(want) {
		t.Errorf("%s: frontmatter has %d keys, want %d (%v vs %v)",
			name, got.Len(), len(want), got.Keys(), want)
		return
	}
	for k, wv := range want {
		gv, ok := got.Get(k)
		if !ok {
			t.Errorf("%s: frontmatter missing key %q", name, k)
			continue
		}
		gj, _ := json.Marshal(gv)
		wj, _ := json.Marshal(wv)
		if string(gj) != string(wj) {
			t.Errorf("%s: frontmatter[%q] = %s, want %s", name, k, gj, wj)
		}
	}
}

func nonNil(xs []string) []string {
	if xs == nil {
		return []string{}
	}
	return xs
}

func nonNilLinks(xs []markdown.Link) []markdown.Link {
	if xs == nil {
		return []markdown.Link{}
	}
	return xs
}

func TestPathConfinementMatchesPython(t *testing.T) {
	var fx pathsFixture
	compat.Load(t, "paths.json", &fx)

	root := t.TempDir()
	// resolve through symlinks the same way the vault does (macOS /tmp is one)
	v, err := New(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(v.Root, "Folder", "Sub"), 0o755); err != nil {
		t.Fatal(err)
	}

	for _, c := range fx.Cases {
		got, err := v.SafePath(c.Rel)
		if c.OK && err != nil {
			t.Errorf("SafePath(%q) rejected with %v, Python accepted it", c.Rel, err)
			continue
		}
		if !c.OK && err == nil {
			t.Errorf("SafePath(%q) accepted, Python rejected it (%s)", c.Rel, c.Error)
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrVault) {
				t.Errorf("SafePath(%q) error is not an ErrVault: %v", c.Rel, err)
			}
			continue
		}
		rel, err := v.RelOf(got)
		if err != nil {
			t.Fatal(err)
		}
		if rel != c.ResolvedRel {
			t.Errorf("SafePath(%q) resolved to %q, want %q", c.Rel, rel, c.ResolvedRel)
		}
	}
}

func TestIsReservedMatchesPython(t *testing.T) {
	var fx pathsFixture
	compat.Load(t, "paths.json", &fx)
	for _, c := range fx.Cases {
		if got := IsReserved(c.Rel); got != c.IsReserved {
			t.Errorf("IsReserved(%q) = %v, want %v", c.Rel, got, c.IsReserved)
		}
	}
}

func TestSlugifyMatchesPython(t *testing.T) {
	var fx pathsFixture
	compat.Load(t, "paths.json", &fx)
	for _, c := range fx.SlugifyCases {
		if got := Slugify(c.Title); got != c.Slug {
			t.Errorf("Slugify(%q) = %q, want %q", c.Title, got, c.Slug)
		}
	}
}

// Traversal must be refused even when the target exists — the check cannot rely
// on the filesystem returning ENOENT.
func TestTraversalRejectedForExistingTargets(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("classified"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := New(filepath.Join(root, "vault"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(v.Root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{
		"../outside/secret.md", "../outside/secret", "a/../../outside/secret.md",
		".grimoire/index.md", "sub/.grimoire/x.md",
	} {
		if _, err := v.SafePath(rel); err == nil {
			t.Errorf("SafePath(%q) was accepted — it escapes the vault", rel)
		}
	}

	// A leading slash means "from the vault root", not "from the filesystem
	// root" — it is stripped, so the path stays confined rather than being
	// rejected. Assert where it actually lands, since accepting it is only safe
	// because of that.
	for _, rel := range []string{"/etc/passwd.md", "/absolute.md"} {
		got, err := v.SafePath(rel)
		if err != nil {
			t.Errorf("SafePath(%q) rejected; Python treats it as vault-relative: %v", rel, err)
			continue
		}
		if !strings.HasPrefix(got, v.Root+string(filepath.Separator)) {
			t.Errorf("SafePath(%q) resolved outside the vault: %q", rel, got)
		}
	}
}
