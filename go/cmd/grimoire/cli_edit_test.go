package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// appendEditor writes a stand-in editor that appends a marker line to the file
// it is given, the way a user adding a line would. The test points EDITOR at it.
func appendEditor(t *testing.T, marker string) string {
	t.Helper()
	script := filepath.Join(t.TempDir(), "append-editor.sh")
	body := "#!/bin/sh\necho '" + marker + "' >> \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", script)
	return script
}

func TestEditOpensTheNoteAndReindexes(t *testing.T) {
	dir := vaultDir(t)
	if _, code := runCmd(t, "new", "Shopping List", "buy milk"); code != 0 {
		t.Fatal("new failed")
	}
	appendEditor(t, "zebrafish marker")

	if _, code := runCmd(t, "edit", "Shopping List"); code != 0 {
		t.Fatalf("edit exit %d", code)
	}
	if !strings.Contains(read(t, dir, "shopping-list.md"), "zebrafish marker") {
		t.Fatal("the editor's change did not reach the note on disk")
	}
	out, code := runCmd(t, "search", "zebrafish")
	if code != 0 || !strings.Contains(out, "shopping-list.md") {
		t.Fatalf("edited text is not searchable (exit %d):\n%s", code, out)
	}
}

func TestEditAcceptsPathAndSlug(t *testing.T) {
	dir := vaultDir(t)
	if _, code := runCmd(t, "new", "Garden Plan", "tomatoes"); code != 0 {
		t.Fatal("new failed")
	}
	appendEditor(t, "by path")
	if _, code := runCmd(t, "edit", "garden-plan.md"); code != 0 {
		t.Fatal("edit by path failed")
	}
	appendEditor(t, "by slug")
	if _, code := runCmd(t, "edit", "garden-plan"); code != 0 {
		t.Fatal("edit by slug failed")
	}
	got := read(t, dir, "garden-plan.md")
	if !strings.Contains(got, "by path") || !strings.Contains(got, "by slug") {
		t.Fatalf("both edits should land in the note:\n%s", got)
	}
}

func TestEditMissingNoteSaysSoAndSuggestsNew(t *testing.T) {
	vaultDir(t)
	appendEditor(t, "never written")
	stderr := captureStderr(t, func() {
		if _, code := runCmd(t, "edit", "Nothing Here"); code == 0 {
			t.Error("editing a missing note should fail")
		}
	})
	if !strings.Contains(stderr, "no note") || !strings.Contains(stderr, "grimoire new") {
		t.Errorf("missing-note message should suggest new:\n%s", stderr)
	}
}

func TestEditWithNoArgumentOpensTodaysDailyNote(t *testing.T) {
	dir := vaultDir(t)
	if _, code := runCmd(t, "daily", "morning standup"); code != 0 {
		t.Fatal("daily failed")
	}
	appendEditor(t, "edited today")
	if _, code := runCmd(t, "edit"); code != 0 {
		t.Fatal("edit with no argument failed")
	}
	day := dailyNotePath(t, dir)
	if !strings.Contains(read(t, dir, day), "edited today") {
		t.Fatalf("today's daily note was not edited (%s)", day)
	}
}

func TestNewAndDailyAcceptEditFlag(t *testing.T) {
	dir := vaultDir(t)
	appendEditor(t, "from new flag")
	if _, code := runCmd(t, "new", "Flagged", "--edit"); code != 0 {
		t.Fatal("new --edit failed")
	}
	if !strings.Contains(read(t, dir, "flagged.md"), "from new flag") {
		t.Fatal("new --edit did not open the editor")
	}

	appendEditor(t, "from daily flag")
	if _, code := runCmd(t, "daily", "--edit"); code != 0 {
		t.Fatal("daily --edit failed")
	}
	if !strings.Contains(read(t, dir, dailyNotePath(t, dir)), "from daily flag") {
		t.Fatal("daily --edit did not open the editor")
	}
}

// dailyNotePath returns the vault-relative path of today's daily note.
func dailyNotePath(t *testing.T, dir string) string {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "journal", "*.md"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one journal note, got %v (%v)", entries, err)
	}
	rel, err := filepath.Rel(dir, entries[0])
	if err != nil {
		t.Fatal(err)
	}
	return rel
}

// captureStderr returns what f writes to stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	f()
	w.Close()
	os.Stderr = old
	b, _ := io.ReadAll(r)
	return string(b)
}

// EDITOR is a shell fragment, so arguments quoted inside it must survive: an
// editor given `--tag "two words"` has to receive that as one argument, with
// the note path after it.
func TestEditKeepsQuotingInsideTheEditorVariable(t *testing.T) {
	dir := vaultDir(t)
	if _, code := runCmd(t, "new", "Quoted Editor", "body"); code != 0 {
		t.Fatal("new failed")
	}
	script := filepath.Join(t.TempDir(), "tag-editor.sh")
	body := "#!/bin/sh\n[ \"$1\" = --tag ] || exit 3\necho \"tagged: $2\" >> \"$3\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VISUAL", script+` --tag "two words"`)
	if _, code := runCmd(t, "edit", "Quoted Editor"); code != 0 {
		t.Fatalf("edit exit %d", code)
	}
	if got := read(t, dir, "quoted-editor.md"); !strings.Contains(got, "tagged: two words") {
		t.Fatalf("the quoted argument was split or lost:\n%s", got)
	}
}
