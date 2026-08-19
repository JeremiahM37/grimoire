package vault

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// linkedVault builds a vault with one note of its own and a directory symlink
// to an outside directory holding two more.
func linkedVault(t *testing.T) (*Vault, string) {
	t.Helper()
	v := testVault(t)
	write(t, filepath.Join(v.Root, "own.md"), "# own")

	outside := t.TempDir()
	write(t, filepath.Join(outside, "linked.md"), "# linked")
	write(t, filepath.Join(outside, "deep", "nested.md"), "# nested")
	write(t, filepath.Join(outside, "not-a-note.txt"), "ignored")
	if err := os.Symlink(outside, filepath.Join(v.Root, "Memory")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return v, outside
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func walked(t *testing.T, v *Vault) []string {
	t.Helper()
	rels, err := v.Walk()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(rels)
	return rels
}

// The default has to stay what it was: a vault that suddenly reads through
// links after an upgrade would index whatever a sync client planted in it.
func TestWalkIgnoresLinkedDirectoriesByDefault(t *testing.T) {
	v, _ := linkedVault(t)
	if got := walked(t, v); !slices.Equal(got, []string{"own.md"}) {
		t.Fatalf("default walk followed a link: %v", got)
	}
}

func TestWalkFollowsLinkedDirectories(t *testing.T) {
	v, _ := linkedVault(t)
	v.Follow = true
	want := []string{"Memory/deep/nested.md", "Memory/linked.md", "own.md"}
	if got := walked(t, v); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// The logical path is the note's identity: reading and writing it must land on
// the linked file, not on a new file inside the vault.
func TestReadWriteThroughLink(t *testing.T) {
	v, outside := linkedVault(t)
	v.Follow = true

	note, err := v.Read("Memory/linked.md")
	if err != nil {
		t.Fatal(err)
	}
	if note.Body != "# linked" {
		t.Fatalf("body %q", note.Body)
	}
	if _, err := v.Write("Memory/linked.md", "# edited", nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(outside, "linked.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "# edited") {
		t.Fatalf("edit did not reach the linked file: %q", raw)
	}
}

// Three ways a link ends badly, all of which must terminate and none of which
// may report a note twice.
func TestWalkRefusesLoopsAndDuplicates(t *testing.T) {
	v := testVault(t)
	v.Follow = true
	write(t, filepath.Join(v.Root, "own.md"), "# own")

	// a link to the vault itself, and one to its parent
	if err := os.Symlink(v.Root, filepath.Join(v.Root, "self")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	parent := filepath.Dir(v.Root)
	if err := os.Symlink(parent, filepath.Join(v.Root, "up")); err != nil {
		t.Fatal(err)
	}
	// a link to a real subdirectory of the vault
	write(t, filepath.Join(v.Root, "sub", "inner.md"), "# inner")
	if err := os.Symlink(filepath.Join(v.Root, "sub"), filepath.Join(v.Root, "also-sub")); err != nil {
		t.Fatal(err)
	}
	// and one that points nowhere
	if err := os.Symlink(filepath.Join(parent, "absent"), filepath.Join(v.Root, "broken")); err != nil {
		t.Fatal(err)
	}

	want := []string{"own.md", "sub/inner.md"}
	if got := walked(t, v); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Two links to the same directory report its notes once, under the first path
// reached, rather than duplicating every note in the index.
func TestWalkVisitsALinkedDirectoryOnce(t *testing.T) {
	v := testVault(t)
	v.Follow = true
	outside := t.TempDir()
	write(t, filepath.Join(outside, "note.md"), "# note")
	for _, name := range []string{"a-link", "b-link"} {
		if err := os.Symlink(outside, filepath.Join(v.Root, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	}
	if got := walked(t, v); !slices.Equal(got, []string{"a-link/note.md"}) {
		t.Fatalf("got %v, want one copy under the first link", got)
	}
}

// A link to a single file needed no new machinery, and must keep working with
// following switched off.
func TestWalkIncludesLinkedFiles(t *testing.T) {
	v := testVault(t)
	outside := t.TempDir()
	write(t, filepath.Join(outside, "note.md"), "# note")
	if err := os.Symlink(filepath.Join(outside, "note.md"), filepath.Join(v.Root, "linked.md")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := walked(t, v); !slices.Equal(got, []string{"linked.md"}) {
		t.Fatalf("got %v", got)
	}
}

func TestFollowSymlinksEnv(t *testing.T) {
	for value, want := range map[string]bool{
		"": false, "0": false, "false": false, "no": false, "off": false,
		"1": true, "true": true, "yes": true,
	} {
		t.Setenv("GRIMOIRE_FOLLOW_SYMLINKS", value)
		if got := followSymlinks(); got != want {
			t.Fatalf("%q: got %v, want %v", value, got, want)
		}
		v, err := New(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if v.Follow != want {
			t.Fatalf("%q: vault follow %v, want %v", value, v.Follow, want)
		}
	}
}
