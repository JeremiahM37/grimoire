package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// memoryDirFixture writes a memory directory: an index linking three files,
// one that restates its summary, one that adds detail, and one about something
// the summary does not mention.
func memoryDirFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	index := "# Memory\n" +
		"- [Ports](ports.md) — the kestrel port is 9000\n" +
		"- [Deploys](deploy.md) — deploy notes\n" +
		"- [Gaps](gaps.md) — when to ask\n" +
		"- [Ports again](ports.md) — the same file, linked twice\n" +
		"- [Missing](missing.md) — no such file\n" +
		"- [External](https://example.com/x.md) — not a memory file\n"
	files := map[string]string{
		"ports.md": "---\nname: ports\ndescription: kestrel port\n---\nThe kestrel port is 9000.\n",
		"deploy.md": "Deploys switch the web directory in release.conf before restarting DETAIL.\n" +
			"Roll back by pointing the symlink at the previous release.\n",
		"gaps.md": "Ask before touching the billing ledger, since nothing there can be undone.\n",
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func auditJSON(t *testing.T, args ...string) (map[string]any, int) {
	t.Helper()
	raw, code := runCmd(t, append([]string{"memory", "loaded-audit", "--json"}, args...)...)
	var out map[string]any
	if code == 0 {
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			t.Fatalf("unreadable output %q: %v", raw, err)
		}
	}
	return out, code
}

func TestLoadedAuditFallsBackWithoutADecisionServer(t *testing.T) {
	vaultDir(t)
	t.Setenv("GRIMOIRE_DECISION_URL", "")
	dir := memoryDirFixture(t)
	out, code := auditJSON(t, "--memory-dir", dir)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out["mode"] != "fallback" || out["files"] != float64(3) || out["missing"] != float64(1) {
		t.Fatalf("mode/files/missing = %v", out)
	}
	if _, ok := out["mean_absent_share"]; !ok {
		t.Fatal("fallback must report its measure")
	}
	counts := out["counts"].(map[string]any)
	paths := out["paths"].(map[string]any)
	if counts["restates"] != float64(1) || counts["independent"] != float64(2) || counts["contradicts"] != float64(0) {
		t.Fatalf("counts = %v", counts)
	}
	if got := paths["restates"].([]any); len(got) != 1 || got[0] != "ports.md" {
		t.Fatalf("the restating file should be ports.md, got %v", got)
	}
	// A file linked twice is judged once.
	if total := counts["restates"].(float64) + counts["refines"].(float64) + counts["independent"].(float64); total != 3 {
		t.Fatalf("judged %v files, want 3", total)
	}
}

func TestLoadedAuditAsksTheDecisionServerTheOneQuestion(t *testing.T) {
	vaultDir(t)
	t.Setenv("GRIMOIRE_DECISION_URL", "")
	dir := memoryDirFixture(t)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			State     string `json:"state"`
			Questions map[string]struct {
				Type         string         `json:"type"`
				Instructions string         `json:"instructions"`
				Criteria     map[string]any `json:"criteria"`
			} `json:"questions"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request %s: %v", raw, err)
		}
		q := body.Questions["beyond"]
		if q.Type != "choice" || q.Instructions != loadedQuestion {
			t.Errorf("question = %+v", q)
		}
		for _, o := range []string{"restates", "refines", "contradicts", "independent"} {
			if _, ok := q.Criteria[o]; !ok {
				t.Errorf("option %s missing from %v", o, q.Criteria)
			}
		}
		label := "restates"
		if strings.Contains(body.State, "DETAIL") {
			label = "refines"
		}
		w.Write([]byte(`{"answers":{"beyond":{"type":"choice","choice":"` + label + `"}}}`))
	}))
	defer srv.Close()

	out, code := auditJSON(t, "--memory-dir", dir, "--decision-url", srv.URL)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if out["mode"] != "decision" || calls != 3 {
		t.Fatalf("mode %v, %d calls (want 3, one per file)", out["mode"], calls)
	}
	counts := out["counts"].(map[string]any)
	if counts["refines"] != float64(1) || counts["restates"] != float64(2) {
		t.Fatalf("counts = %v", counts)
	}
	if _, ok := out["mean_absent_share"]; ok {
		t.Fatal("decision mode must not report the fallback measure")
	}
}

func TestLoadedAuditStopsWhenTheDecisionServerFails(t *testing.T) {
	vaultDir(t)
	t.Setenv("GRIMOIRE_DECISION_URL", "")
	dir := memoryDirFixture(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer down.Close()
	if _, code := auditJSON(t, "--memory-dir", dir, "--decision-url", down.URL); code == 0 {
		t.Fatal("a failing decision server must not produce a report")
	}

	unknown := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"beyond":{"type":"choice","choice":"poem"}}}`))
	}))
	defer unknown.Close()
	if _, code := auditJSON(t, "--memory-dir", dir, "--decision-url", unknown.URL); code == 0 {
		t.Fatal("an answer outside the four options must fail the audit")
	}
}

func TestLoadedAuditNeedsAnIndex(t *testing.T) {
	vaultDir(t)
	t.Setenv("GRIMOIRE_DECISION_URL", "")
	if _, code := auditJSON(t, "--memory-dir", t.TempDir()); code == 0 {
		t.Fatal("a directory without MEMORY.md must fail")
	}
}
