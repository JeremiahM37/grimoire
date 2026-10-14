package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The anchors and refs commands are clients of admin routes, run here against
// a temp vault through the in-process handler, exactly as the other memory
// commands are tested.

func TestMemoryAnchorsReportsTheClaimThatNoLongerHolds(t *testing.T) {
	dir := vaultDir(t)
	note := "- **2026-10-11 09:00 · agent** — the router is 10.0.0.1 <!--m id=aaa111bbb222 " +
		"anc=file_contains;path=/nonexistent-anchor-dir/lan.txt;text=10.0.0.1-->\n" +
		"- **2026-10-11 09:00 · agent** — the deploy happened <!--m id=ccc333ddd444 evt=1-->\n"
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory", "net.md"), []byte("# Net\n\n"+note), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runCmd(t, "memory", "anchors")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	for _, want := range []string{"events 1", "claims 1", "anchored claims 1", "fail 1", "memory/net.md", "file_contains"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "the router is") {
		t.Errorf("anchor report printed fact text:\n%s", out)
	}

	raw, code := runCmd(t, "memory", "anchors", "--json")
	var got map[string]any
	if code != 0 || json.Unmarshal([]byte(raw), &got) != nil {
		t.Fatalf("--json: exit %d, output %q", code, raw)
	}
	if _, ok := got["failing"]; !ok {
		t.Fatalf("--json lacks the failing list: %s", raw)
	}
}

func TestMemoryRefsListsDeadPathsAndUnits(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := vaultDir(t)
	if err := os.WriteFile(filepath.Join(dir, "runbook.md"),
		[]byte("Run ~/gone-tool.sh and the unit missing-thing.service.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, code := runCmd(t, "memory", "refs")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "dead 2") || !strings.Contains(out, "runbook.md -> "+filepath.Join(home, "gone-tool.sh")) ||
		!strings.Contains(out, "missing-thing.service") {
		t.Fatalf("refs output:\n%s", out)
	}
}

func TestMemoryAnchorsAndRefsAreInTheHelp(t *testing.T) {
	vaultDir(t)
	out, _ := runCmd(t, "memory", "help")
	for _, want := range []string{"grimoire memory anchors", "grimoire memory refs"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
}

// `memory anchors history` lists the recorded results and the changes between
// them. An empty history is a valid answer, not an error, and --json passes the
// server's report through unchanged.
func TestMemoryAnchorsHistoryListsResultsAndChanges(t *testing.T) {
	dir := vaultDir(t)
	if err := os.MkdirAll(filepath.Join(dir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, code := runCmd(t, "memory", "anchors", "history")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if !strings.Contains(out, "results 0") || !strings.Contains(out, "changes 0") {
		t.Fatalf("empty history output:\n%s", out)
	}

	raw, code := runCmd(t, "memory", "anchors", "history", "--fact", "aaa111bbb222", "--json")
	if code != 0 {
		t.Fatalf("json exit %d: %s", code, raw)
	}
	var got struct {
		Rows        []json.RawMessage `json:"rows"`
		Transitions []json.RawMessage `json:"transitions"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("json is not the server report: %v\n%s", err, raw)
	}
}
