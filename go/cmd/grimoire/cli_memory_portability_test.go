package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The portable export and import from the shell. These assert on the files and
// the printed summaries, the way someone moving their memory would see them.

func TestMemoryExportWritesTheDocumentedFormat(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "remember", "the deploy needs a VPN reset", "--topic", "ops", "--category", "fact"); code != 0 {
		t.Fatal("remember failed")
	}
	out := filepath.Join(t.TempDir(), "memory.jsonl")
	if _, code := runCmd(t, "memory", "export", "--out", out); code != 0 {
		t.Fatalf("export = %d", code)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], `{"format":"grimoire-memory","version":1`) ||
		!strings.Contains(lines[1], "the deploy needs a VPN reset") {
		t.Fatalf("export file = %q", raw)
	}
	if info, _ := os.Stat(out); info.Mode().Perm() != 0o600 {
		t.Errorf("export mode = %v, want 0600: it holds the whole memory", info.Mode().Perm())
	}
}

func TestMemoryExportMarkdownToStdout(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "the box is fast", "--topic", "ops", "--category", "fact")
	out, code := runCmd(t, "memory", "export", "--format", "markdown")
	if code != 0 || !strings.Contains(out, "## fact") || !strings.Contains(out, "- the box is fast") {
		t.Fatalf("markdown export = %d %q", code, out)
	}
}

func TestMemoryImportDryRunThenImportIsIdempotent(t *testing.T) {
	vaultDir(t)
	src := filepath.Join(t.TempDir(), "mem0.json")
	raw, err := os.ReadFile("../../internal/memport/testdata/mem0.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	dry, code := runCmd(t, "memory", "import", src, "--dry-run")
	if code != 0 || !strings.Contains(dry, "dry run (mem0)") || !strings.Contains(dry, "2 would be written") {
		t.Fatalf("dry run = %d %q", code, dry)
	}
	exp, _ := runCmd(t, "memory", "export")
	if strings.Contains(exp, "dark mode") {
		t.Fatal("a dry run wrote facts")
	}

	out, code := runCmd(t, "memory", "import", src)
	if code != 0 || !strings.Contains(out, "imported 2 of 3 (mem0)") {
		t.Fatalf("import = %d %q", code, out)
	}
	again, _ := runCmd(t, "memory", "import", src)
	if !strings.Contains(again, "imported 0 of 3") || !strings.Contains(again, "2 already on file") {
		t.Fatalf("re-run = %q", again)
	}
}

func TestMemoryImportRoundTripFromExport(t *testing.T) {
	vaultDir(t)
	runCmd(t, "remember", "backups run at 03:00", "--topic", "ops", "--agent", "cli")
	file := filepath.Join(t.TempDir(), "all.jsonl")
	if _, code := runCmd(t, "memory", "export", "--out", file); code != 0 {
		t.Fatal("export failed")
	}
	out, code := runCmd(t, "memory", "import", file, "--from", "grimoire")
	if code != 0 || !strings.Contains(out, "imported 0 of 1 (grimoire)") {
		t.Fatalf("re-importing an export into its own vault = %d %q", code, out)
	}
}

func TestMemoryImportRejectsUnknownFiles(t *testing.T) {
	vaultDir(t)
	src := filepath.Join(t.TempDir(), "junk.txt")
	if err := os.WriteFile(src, []byte("hello there"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := runCmd(t, "memory", "import", src); code == 0 {
		t.Fatal("an unrecognised file was imported")
	}
}
