package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// The annotate command is a client of POST /api/memory/annotate. These tests
// run it against a temp vault. The fact's id is derived from its own text, as
// the store derives it, so the fact is not hand-written.

func annotateFixture(t *testing.T) (dir, id string) {
	t.Helper()
	dir = vaultDir(t)
	const stamp, agent, text = "2026-10-11 09:00", "agent", "the cutover is planned"
	id = memory.DeriveID(stamp, agent, text)
	line := "- **" + stamp + " · " + agent + "** — " + text + " <!--m id=" + id + "-->\n"
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "ops.md"), []byte("# Ops\n\n"+line), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, id
}

func writeAnnotateFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "annotate.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMemoryAnnotateDryRunByDefaultAndAppliesWithFlag(t *testing.T) {
	dir, id := annotateFixture(t)
	file := writeAnnotateFile(t, `{"items":[{"id":"`+id+`","path":"memory/ops.md","event":true}]}`)
	note := filepath.Join(dir, "memory", "ops.md")

	out, code := runCmd(t, "memory", "annotate", "--file", file)
	if code != 0 || !strings.Contains(out, "changed") || !strings.Contains(out, "dry run: nothing was written") {
		t.Fatalf("dry run: exit %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(note); strings.Contains(string(raw), "evt=1") {
		t.Fatal("a dry run wrote the note")
	}

	out, code = runCmd(t, "memory", "annotate", "--file", file, "--apply")
	if code != 0 || strings.Contains(out, "dry run") {
		t.Fatalf("apply: exit %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(note); !strings.Contains(string(raw), "evt=1") {
		t.Fatalf("apply did not write the event flag:\n%s", raw)
	}
}

func TestMemoryAnnotateNeedsAFileOfItems(t *testing.T) {
	vaultDir(t)
	// Errors go to stderr, so the exit status is what these check.
	if _, code := runCmd(t, "memory", "annotate"); code == 0 {
		t.Fatal("annotate without --file succeeded")
	}
	bad := writeAnnotateFile(t, `{"items":[]}`)
	if _, code := runCmd(t, "memory", "annotate", "--file", bad); code == 0 {
		t.Fatal("annotate with no items succeeded")
	}
}

func TestMemoryAnnotateJSONAndHelp(t *testing.T) {
	_, id := annotateFixture(t)
	file := writeAnnotateFile(t, `{"items":[{"id":"`+id+`","path":"memory/ops.md","host":"mediaserver"}]}`)
	raw, code := runCmd(t, "memory", "annotate", "--file", file, "--json")
	if code != 0 || !strings.Contains(raw, `"dry_run":true`) || !strings.Contains(raw, `"changed":true`) {
		t.Fatalf("--json: exit %d: %s", code, raw)
	}
	help, _ := runCmd(t, "memory", "help")
	if !strings.Contains(help, "grimoire memory annotate") {
		t.Fatalf("help lacks annotate:\n%s", help)
	}
}

// When a server answers, annotate writes through it and says so. The server is
// a real handler on the temp vault, so the write is the server's, not the CLI's.
func TestMemoryAnnotateUsesTheLiveServerWhenOneAnswers(t *testing.T) {
	dir, id := annotateFixture(t)
	e, err := newEnv(false)
	if err != nil {
		t.Fatal(err)
	}
	defer e.close()
	srv := httptest.NewServer(e.handler)
	defer srv.Close()
	t.Setenv("GRIMOIRE_URL", srv.URL)

	file := writeAnnotateFile(t, `{"items":[{"id":"`+id+`","path":"memory/ops.md","event":true}]}`)
	out, code := runCmd(t, "memory", "annotate", "--file", file, "--apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "via live server at "+srv.URL) || strings.Contains(out, "in-process") {
		t.Fatalf("output does not say the live server wrote it:\n%s", out)
	}
	if !strings.Contains(out, "notes written 1") {
		t.Fatalf("output does not count the note write:\n%s", out)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "memory", "ops.md")); !strings.Contains(string(raw), "evt=1") {
		t.Fatalf("the live server did not write the event flag:\n%s", raw)
	}
}

// With nothing listening, annotate opens the vault itself and says it did.
func TestMemoryAnnotateFallsBackInProcessWhenNoServerAnswers(t *testing.T) {
	dir, id := annotateFixture(t)
	file := writeAnnotateFile(t, `{"items":[{"id":"`+id+`","path":"memory/ops.md","event":true}]}`)
	out, code := runCmd(t, "memory", "annotate", "--file", file, "--apply")
	if code != 0 || !strings.Contains(out, "in-process: no server answered") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "memory", "ops.md")); !strings.Contains(string(raw), "evt=1") {
		t.Fatalf("the fallback did not write:\n%s", raw)
	}
}

// Only a refused connection means no server answered. A server that answers
// with an error is an answer, so the caller must not write a second time.
func TestPostLiveSeparatesNoServerFromAnAnswer(t *testing.T) {
	vaultDir(t)
	if _, _, err := postLive("/api/memory/annotate", map[string]any{}); !errors.Is(err, errNoServer) {
		t.Fatalf("nothing listening: err = %v, want errNoServer", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"stored":true}`))
	}))
	defer srv.Close()
	t.Setenv("GRIMOIRE_URL", srv.URL)
	status, body, err := postLive("/api/memory/procedure", map[string]any{})
	if err != nil || status != http.StatusCreated || !strings.Contains(body, "stored") {
		t.Fatalf("answered: status %d body %q err %v", status, body, err)
	}
}
