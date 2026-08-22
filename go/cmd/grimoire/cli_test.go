package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

// The CLI writes to the vault with no server running, which is the whole point
// of it — so these tests drive the commands and then assert on the FILES, the
// same way a user would check.

func vaultDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("GRIMOIRE_VAULT", dir)
	return dir
}

// runCmd executes a command and returns its stdout.
func runCmd(t *testing.T, args ...string) (string, int) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	handled, code := runCLI(args)
	w.Close()
	os.Stdout = old
	if !handled {
		t.Fatalf("%v was not handled as a command", args)
	}
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return sb.String(), code
}

func read(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(b)
}

func TestNewCreatesASluggedNote(t *testing.T) {
	dir := vaultDir(t)
	out, code := runCmd(t, "new", "My First Note", "hello", "world")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if strings.TrimSpace(out) != "my-first-note.md" {
		t.Errorf("printed %q", out)
	}
	body := read(t, dir, "my-first-note.md")
	if !strings.Contains(body, "title: My First Note") || !strings.Contains(body, "hello world") {
		t.Errorf("note:\n%s", body)
	}
}

func TestDailyCreatesThenAppends(t *testing.T) {
	dir := vaultDir(t)
	out, _ := runCmd(t, "daily")
	// with no text it prints the path, so `$(grimoire daily)` opens in an editor
	if !strings.Contains(out, "journal/") {
		t.Errorf("printed %q", out)
	}
	runCmd(t, "daily", "first thing")
	runCmd(t, "daily", "second thing")

	matches, _ := filepath.Glob(filepath.Join(dir, "journal", "*.md"))
	if len(matches) != 1 {
		t.Fatalf("journal files = %v", matches)
	}
	body := read(t, dir, filepath.Join("journal", filepath.Base(matches[0])))
	if !strings.Contains(body, "- first thing") || !strings.Contains(body, "- second thing") {
		t.Errorf("daily note:\n%s", body)
	}
	if strings.Count(body, "tags:") != 1 {
		t.Errorf("frontmatter duplicated on append:\n%s", body)
	}
}

func TestCaptureRefusesEmptyInput(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "capture"); code == 0 {
		t.Error("empty capture succeeded — it would write a blank note")
	}
}

func TestSeedDemoThenSearchAndLs(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "seed-demo"); code != 0 {
		t.Fatal("seed-demo failed")
	}
	out, code := runCmd(t, "search", "deploy")
	if code != 0 {
		t.Fatalf("search exit %d", code)
	}
	if !strings.Contains(out, "deployment-runbook.md") {
		t.Errorf("search output:\n%s", out)
	}

	// operators must behave here exactly as they do over HTTP
	out, _ = runCmd(t, "search", "tag:onboarding")
	if !strings.Contains(out, "team-onboarding.md") || strings.Contains(out, "monitoring.md") {
		t.Errorf("tag: operator output:\n%s", out)
	}

	out, _ = runCmd(t, "ls", "--tag", "ops")
	if !strings.Contains(out, "monitoring.md") || strings.Contains(out, "team-onboarding.md") {
		t.Errorf("ls --tag output:\n%s", out)
	}
}

func TestIngestSkipsNonTextAndHiddenFiles(t *testing.T) {
	dir := vaultDir(t)
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, s string) {
		if err := os.WriteFile(filepath.Join(src, p), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Doc One.md", "# Doc One\n\ncontent\n")
	write("sub/notes.txt", "plain text\n")
	write("photo.png", "binary-ish\n")
	write(".git/config", "[core]\n")

	out, code := runCmd(t, "ingest", src, "--into", "imported")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "ingested 2 file(s)") {
		t.Errorf("ingest output: %s", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "imported", "doc-one.md")); err != nil {
		t.Error("markdown not imported")
	}
	if _, err := os.Stat(filepath.Join(dir, "imported", "sub", "notes.md")); err != nil {
		t.Error("nested text file not imported")
	}
	// a title survives from the ORIGINAL filename, not the slug
	if body := read(t, dir, "imported/doc-one.md"); !strings.Contains(body, "title: Doc One") {
		t.Errorf("title lost:\n%s", body)
	}
}

func TestExportWritesStaticHTML(t *testing.T) {
	vaultDir(t)
	runCmd(t, "seed-demo")
	out := filepath.Join(t.TempDir(), "site")
	if s, code := runCmd(t, "export", "--out", out); code != 0 {
		t.Fatalf("exit %d: %s", code, s)
	}
	index, err := os.ReadFile(filepath.Join(out, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "<html") && !strings.Contains(string(index), "<a ") {
		t.Errorf("index.html looks empty:\n%s", index[:min(len(index), 200)])
	}
	note, err := os.ReadFile(filepath.Join(out, "deployment-runbook.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(note), "Rolling back a bad deploy") {
		t.Error("exported note lost its content")
	}
}

func TestOpenPrintsRawAndRejectsEscapes(t *testing.T) {
	vaultDir(t)
	runCmd(t, "new", "Thing", "body text")
	out, code := runCmd(t, "open", "thing.md")
	if code != 0 || !strings.Contains(out, "body text") || !strings.Contains(out, "title: Thing") {
		t.Errorf("open (%d):\n%s", code, out)
	}
	if _, code := runCmd(t, "open", "../../etc/passwd"); code == 0 {
		t.Error("path escape accepted")
	}
}

func TestUnknownCommandIsRejectedNotServed(t *testing.T) {
	// falling through to serve would be the dangerous failure: a typo would
	// silently start a server instead of reporting the mistake
	if handled, code := runCLI([]string{"nonsense"}); !handled || code == 0 {
		t.Errorf("handled=%v code=%d", handled, code)
	}
	if handled, _ := runCLI([]string{"serve"}); handled {
		t.Error("serve should fall through to the server")
	}
	if handled, _ := runCLI(nil); handled {
		t.Error("no arguments should fall through to the server")
	}
}

func TestAgentSetupPointsAtTheGoMCPBinary(t *testing.T) {
	out, code := runCmd(t, "agent-setup", "http://example:9111")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "grimoire-mcp") {
		t.Errorf("MCP command missing from:\n%s", out)
	}
	if strings.Contains(out, "server.mcp_server") {
		t.Error("still advertising the Python module")
	}
	if !strings.Contains(out, "http://example:9111") {
		t.Error("API url not threaded through")
	}
	// The name, not just the value. This printed GRIMOIRE_API for several
	// releases — a variable nothing reads — so the config it emitted pointed a
	// correctly-specified server at the default address and silently ignored
	// the URL the user passed in. The assertion above still passed throughout,
	// because the URL was present; it was attached to the wrong key.
	if !strings.Contains(out, mcp.EnvURL) {
		t.Errorf("agent-setup must emit %s, the var grimoire-mcp reads:\n%s", mcp.EnvURL, out)
	}
	if strings.Contains(out, "GRIMOIRE_API") {
		t.Error("GRIMOIRE_API is read by nothing; emitting it produces a dead mount")
	}
}
