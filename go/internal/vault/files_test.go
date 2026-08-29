package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

func testVault(t *testing.T) *Vault {
	t.Helper()
	v, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func pinTime(t *testing.T) {
	t.Helper()
	old := Now
	Now = func() time.Time { return time.Date(2026, 8, 14, 10, 30, 0, 0, time.Local) }
	t.Cleanup(func() { Now = old })
}

func TestWriteReadRoundTrip(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Hello")

	n, err := v.Write("hello.md", "# Hello\n\nbody\n", fm)
	if err != nil {
		t.Fatal(err)
	}
	if n.Title != "Hello" {
		t.Errorf("title = %q", n.Title)
	}
	got, err := v.Read("hello")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Hello" || !strings.Contains(got.Body, "body") {
		t.Errorf("round trip lost content: %+v", got)
	}
	if got.Frontmatter.StringVal("created") == "" || got.Frontmatter.StringVal("updated") == "" {
		t.Error("created/updated not stamped")
	}
}

// The BYO-vault guarantee at file level: rewriting a note must not disturb
// frontmatter another app owns.
func TestWritePreservesForeignFrontmatter(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	original := "---\ntitle: Old\nobsidian:\n  cssclass: wide\n  nested:\n    deep: true\ncustom: kept\n---\nbody\n"
	p := filepath.Join(v.Root, "note.md")
	if err := os.WriteFile(p, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", "New")
	if _, err := v.Write("note.md", "new body\n", fm); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	text := string(raw)
	for _, want := range []string{"obsidian:", "cssclass: wide", "nested:", "deep: true"} {
		if !strings.Contains(text, want) {
			t.Errorf("foreign frontmatter %q was lost:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "title: New") {
		t.Errorf("managed key not updated:\n%s", text)
	}
}

// A crash mid-write must not leave a truncated note, and the temp file must
// not linger in the vault where it would be indexed.
func TestWriteIsAtomic(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	if _, err := v.Write("a.md", "body\n", nil); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(v.Root)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestRenameRefusesToClobber(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	if _, err := v.Write("a.md", "a\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Write("b.md", "b\n", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Rename("a.md", "b.md"); err == nil {
		t.Error("rename clobbered an existing note")
	}
	if _, err := v.Rename("missing.md", "c.md"); err == nil {
		t.Error("renaming a missing note should fail")
	}
	newRel, err := v.Rename("a.md", "sub/c.md")
	if err != nil {
		t.Fatal(err)
	}
	if newRel != "sub/c.md" {
		t.Errorf("new rel = %q", newRel)
	}
}

func TestWalkSkipsReservedDirs(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	for _, rel := range []string{"a.md", "sub/b.md"} {
		if _, err := v.Write(rel, "x\n", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range ReservedDirs {
		if err := os.MkdirAll(filepath.Join(v.Root, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(v.Root, dir, "hidden.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := v.Walk()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("walk returned %v, want the 2 real notes only", got)
	}
	for _, g := range got {
		if IsReserved(g) {
			t.Errorf("walk returned a reserved path: %s", g)
		}
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	pinTime(t)
	v := testVault(t)
	if _, err := v.Write("gone.md", "x\n", nil); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete("gone.md"); err != nil {
		t.Fatal(err)
	}
	if err := v.Delete("gone.md"); err != nil {
		t.Errorf("deleting a missing note should be a no-op, got %v", err)
	}
}

// A vault can span two filesystems: GRIMOIRE_FOLLOW_SYMLINKS makes a linked
// directory part of it, and that directory frequently lives on another device.
// os.Rename cannot cross that boundary, so moving a note out of one failed with
// "invalid cross-device link" and a 500 — which reads as a broken server rather
// than a mount that straddles a filesystem.
func TestCopyAcrossDevicesPreservesContentAndMode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.md")
	dst := filepath.Join(dir, "sub", "dst.md")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "# Note\n\nbody with unicode — ✓ and a tab\there\n"
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := copyAcrossDevices(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("content changed across the copy:\n%q", got)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600 — a private note must not widen on a move",
			info.Mode().Perm())
	}
	// The source is left for the caller to remove, so an interrupted move
	// cannot lose the note.
	if _, err := os.Stat(src); err != nil {
		t.Error("copyAcrossDevices removed the source; the caller must do that " +
			"only after the copy is known good")
	}
}

// No temporary files may survive a successful copy.
func TestCopyAcrossDevicesLeavesNoTemporaries(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.md")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyAcrossDevices(src, filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".grimoire-move-") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
