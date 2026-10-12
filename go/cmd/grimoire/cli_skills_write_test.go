package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProcFile writes a procedure request to a temp file and returns its path.
func writeProcFile(t *testing.T, body map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "proc.json")
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSkillsWriteStoresAProcedureCheckSeesIt(t *testing.T) {
	agentHome(t)
	vaultDir(t)
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRIMOIRE_MEMORY_CANONICAL_DIR", store)
	ready := filepath.Join(t.TempDir(), "ready.txt")
	if err := os.WriteFile(ready, []byte("kestrel ready\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	req := map[string]any{
		"name": "Rotate kestrel key", "when": "rotating the kestrel key",
		"steps": []string{"make rotate", "make verify"},
		"pre":   []map[string]any{{"type": "file_exists", "path": ready}},
		"post":  []map[string]any{{"type": "file_contains", "path": ready, "text": "kestrel ready"}},
	}
	file := writeProcFile(t, req)

	out, code := runCmd(t, "skills", "write", "--file", file)
	if code != 0 || !strings.Contains(out, "saved procedure_rotate-kestrel-key.md") {
		t.Fatalf("write = %d:\n%s", code, out)
	}
	if !strings.Contains(out, "pre") || !strings.Contains(out, "file_exists") {
		t.Errorf("write should list the checks it ran:\n%s", out)
	}

	raw, code := runCmd(t, "skills", "check", "--cwd", cwd, "--json")
	if code != 0 {
		t.Fatalf("check = %d:\n%s", code, raw)
	}
	var rep struct {
		Skills []struct {
			Title   string   `json:"title"`
			Applies bool     `json:"applies"`
			Unmet   []string `json:"unmet"`
		} `json:"skills"`
	}
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatalf("check --json not JSON: %v\n%s", err, raw)
	}
	if len(rep.Skills) != 1 || !rep.Skills[0].Applies {
		t.Fatalf("written procedure should apply while its pre check holds: %+v", rep.Skills)
	}

	// Remove the pre check's file: the procedure stops applying and says why.
	if err := os.Remove(ready); err != nil {
		t.Fatal(err)
	}
	raw, _ = runCmd(t, "skills", "check", "--cwd", cwd, "--json")
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Skills) != 1 || rep.Skills[0].Applies || len(rep.Skills[0].Unmet) != 1 ||
		!strings.HasPrefix(rep.Skills[0].Unmet[0], "anchor:") {
		t.Fatalf("missing pre should be named as unmet: %+v", rep.Skills)
	}
}

func TestSkillsWriteRefusesAFailingPreAndWritesNothing(t *testing.T) {
	agentHome(t)
	vaultDir(t)
	store := filepath.Join(t.TempDir(), "store")
	if err := os.MkdirAll(store, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRIMOIRE_MEMORY_CANONICAL_DIR", store)
	file := writeProcFile(t, map[string]any{
		"name": "Migrate the ledger", "when": "migrating the ledger",
		"steps": []string{"make migrate", "make check"},
		"pre":   []map[string]any{{"type": "file_exists", "path": filepath.Join(store, "absent.txt")}},
		"post":  []map[string]any{{"type": "dir_exists", "path": store}},
	})
	_, code := runCmd(t, "skills", "write", "--file", file)
	if code == 0 {
		t.Fatal("a failing pre check must exit non-zero")
	}
	if entries, _ := filepath.Glob(filepath.Join(store, "*.md")); len(entries) != 0 {
		t.Fatalf("a refused procedure left notes behind: %v", entries)
	}
}

func TestSkillsWriteNeedsAFileAndAnObject(t *testing.T) {
	agentHome(t)
	vaultDir(t)
	if _, code := runCmd(t, "skills", "write"); code == 0 {
		t.Fatal("write without --file must fail")
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte(`["not", "an", "object"]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := runCmd(t, "skills", "write", "--file", bad); code == 0 {
		t.Fatal("a non-object body must fail")
	}
}
