package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func storeNote(t *testing.T, dir, name, typ, desc, body string) {
	t.Helper()
	text := "---\nname: " + strings.TrimSuffix(name, ".md") + "\ndescription: " + desc + "\nmetadata:\n  type: " + typ + "\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRememberCarriesKindAndWarnsWithoutBlocking(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "rules", "infer": false, "kind": "rule",
		"text": "Never force-push to main."})
	ws, _ := out["warnings"].([]any)
	if len(ws) < 2 {
		t.Fatalf("a rule with no why/how should warn, got %v", out["warnings"])
	}
	facts := recallFacts(t, h, "?q=force-push")
	if len(facts) != 1 || facts[0]["kind"] != "rule" || facts[0]["category"] != "rule" {
		t.Fatalf("recall = %v", facts)
	}
	clean := remember(t, h, map[string]any{"topic": "rules2", "infer": false, "kind": "rule",
		"text": "Never force-push to main because it rewrote shared history once. Apply when pushing."})
	if clean["warnings"] != nil {
		t.Errorf("a well-formed rule should not warn: %v", clean["warnings"])
	}
	if w := do(t, h, "POST", "/api/memory", map[string]any{"text": "x", "kind": "bogus"}); w.Code != 400 {
		t.Errorf("bad kind = %d", w.Code)
	}
}

func TestMemoryCoreRulesAndPointers(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	dir := t.TempDir()
	for _, n := range []string{"a", "b", "c"} {
		storeNote(t, dir, "feedback_"+n+".md", "feedback", "rule "+n+" never do "+n, "Why: x. How to apply: y.")
	}
	for i, topic := range []string{"deploy", "deploy", "network", "network", "network"} {
		storeNote(t, dir, "project_"+string(rune('a'+i))+".md", "project", topic+" thing "+string(rune('a'+i)), "x")
	}
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/memory/core?budget=4000", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	core, _ := out["core"].(string)
	if out["rules"].(float64) != 3 || out["pointers"].(float64) < 1 || !strings.Contains(core, "recall '") {
		t.Fatalf("%v", out)
	}
	if out["bytes"].(float64) > 4000 {
		t.Errorf("over budget: %v", out["bytes"])
	}
	raw := do(t, h, "GET", "/api/memory/core?raw=1", nil)
	if !strings.Contains(raw.Body.String(), "## Rules") {
		t.Errorf("raw = %q", raw.Body)
	}
}

func TestDreamVerifiesDueProceduresAndRecallShowsVerify(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	dir := t.TempDir()
	storeNote(t, dir, "reference_deploy.md", "project", "How to deploy",
		"1. run /opt/definitely-not-here-xyz/deploy.sh\n2. restart\n3. check")
	storeNote(t, dir, "reference_ok.md", "project", "How to look around",
		"1. cd "+dir+"/\n2. ls\n3. done")
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}
	rep, err := s.Dream(t.Context(), false, false)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, f := range rep.Findings {
		if f.Check == "procedure_verify" {
			failures++
			if !strings.Contains(f.Message, "definitely-not-here-xyz") {
				t.Errorf("message = %s", f.Message)
			}
		}
	}
	if failures != 1 {
		t.Fatalf("want 1 procedure_verify finding, got %d: %+v", failures, rep.Findings)
	}
	st := s.procStates()
	if !st["note:reference_deploy.md"].Failed || st["note:reference_ok.md"].Failed || st["note:reference_ok.md"].EveryDays != 14 {
		t.Fatalf("state = %+v", st)
	}
	// Notes are never touched.
	if b, _ := os.ReadFile(filepath.Join(dir, "reference_deploy.md")); !strings.Contains(string(b), "restart") {
		t.Error("note changed")
	}
	// A stored procedure fact whose state failed renders verify on recall.
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "kind": "procedure",
		"text": "How to rebuild: run /opt/gone-for-good-qq/rebuild.sh then restart"})
	if _, err := s.Dream(t.Context(), false, false); err != nil {
		t.Fatal(err)
	}
	facts := recallFacts(t, h, "?q=rebuild")
	if len(facts) != 1 || facts[0]["kind"] != "procedure" || !strings.Contains(facts[0]["verify"].(string), "gone-for-good") {
		t.Fatalf("recall = %v", facts)
	}
}

func TestContextItemsCarryKind(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "rules", "infer": false, "kind": "rule",
		"text": "Never deploy on fridays because the on-call is thin. Apply when scheduling releases."})
	w := do(t, h, "GET", "/api/memory/context?q=deploy+fridays+release&format=json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `\"kind\":\"rule\"`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}
