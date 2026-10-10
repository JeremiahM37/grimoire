package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMemoryLinkStatusIndexUnlink(t *testing.T) {
	vaultDir(t)
	root := t.TempDir()
	store := filepath.Join(root, "store")
	os.MkdirAll(store, 0o755)
	os.WriteFile(filepath.Join(store, "feedback_a.md"), []byte("---\nname: a\ndescription: never do a\nmetadata:\n  type: feedback\n---\nbody"), 0o644)
	os.WriteFile(filepath.Join(store, "project_b.md"), []byte("---\nname: b\ndescription: state of b\nmetadata:\n  type: project\n---\nbody"), 0o644)

	agentDir := filepath.Join(root, "claude", "memory")
	if out, code := runCmd(t, "memory", "link", "--agent", "claude-code", "--path", agentDir, "--dir", store); code != 0 {
		t.Fatalf("link = %d: %s", code, out)
	}
	if fi, err := os.Lstat(agentDir); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not a symlink: %v", err)
	}
	agentsMD := filepath.Join(root, "AGENTS.md")
	os.WriteFile(agentsMD, []byte("# mine\n"), 0o644)
	if out, code := runCmd(t, "memory", "link", "--agent", "codex", "--path", agentsMD, "--dir", store); code != 0 {
		t.Fatalf("file link = %d: %s", code, out)
	}
	out, _ := runCmd(t, "memory", "status", "--dir", store)
	if !strings.Contains(out, "2 notes") || !strings.Contains(out, "linked") || !strings.Contains(out, "block") {
		t.Errorf("status:\n%s", out)
	}

	// A dry run prints the core and writes nothing.
	out, _ = runCmd(t, "memory", "index", "--dir", store)
	if !strings.Contains(out, "## Rules") || !strings.Contains(out, "never do a") {
		t.Errorf("index:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(store, "MEMORY.md")); err == nil {
		t.Fatal("dry run wrote MEMORY.md")
	}
	os.WriteFile(filepath.Join(store, "MEMORY.md"), []byte("- old\n"), 0o644)
	if out, code := runCmd(t, "memory", "index", "--dir", store, "--write"); code != 0 || !strings.Contains(out, "backup:") {
		t.Fatalf("write = %d: %s", code, out)
	}
	if b, _ := os.ReadFile(filepath.Join(store, "MEMORY.md")); !strings.Contains(string(b), "grimoire:generated-index") {
		t.Errorf("MEMORY.md = %s", b)
	}
	bak, _ := filepath.Glob(filepath.Join(store, "MEMORY.md.grimoire-bak-*"))
	if len(bak) != 1 {
		t.Errorf("backups = %v", bak)
	}

	if out, code := runCmd(t, "memory", "unlink", "--agent", "claude-code", "--dir", store); code != 0 {
		t.Fatalf("unlink = %d: %s", code, out)
	}
	if fi, _ := os.Lstat(agentDir); fi == nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Error("unlink left a symlink")
	}
}

func TestMemoryReplayCLIHoldsALossyChange(t *testing.T) {
	vaultDir(t)
	if out, code := runCmd(t, "new", "Kestrel rule", "Never push to the kestrel repository without being asked. Kestrel pushes need a pull request."); code != 0 {
		t.Fatalf("new = %d: %s", code, out)
	}
	work := t.TempDir()
	cases := `{"cases":[
	 {"text":"should I push to the kestrel repository","min_rel":0.3,"expect":["note:kestrel-rule.md"]},
	 {"text":"can I push the kestrel repository now","min_rel":0.3,"expect":["note:kestrel-rule.md"]},
	 {"text":"the kestrel repository needs a push","min_rel":0.3,"expect":["note:kestrel-rule.md"]}]}`
	seed := filepath.Join(work, "seed.json")
	os.WriteFile(seed, []byte(cases), 0o644)
	if out, code := runCmd(t, "memory", "replay", "seed", seed); code != 0 || !strings.Contains(out, `"added":3`) {
		t.Fatalf("seed = %d: %s", code, out)
	}
	if out, _ := runCmd(t, "memory", "replay"); !strings.Contains(out, `"live":0`) || !strings.Contains(out, `"seed":3`) {
		t.Fatalf("stats: %s", out)
	}

	bad := filepath.Join(work, "bad.md")
	os.WriteFile(bad, []byte("Quarterly invoices are reconciled by the finance team on Tuesdays."), 0o644)
	out, code := runCmd(t, "memory", "replay", "--diff", "--note", "kestrel-rule.md", "--with", bad)
	if code != 2 || !strings.Contains(out, "HOLD") || !strings.Contains(out, "lost 3 useful recall") {
		t.Fatalf("a lossy rewrite must be held (exit 2): %d\n%s", code, out)
	}
	good := filepath.Join(work, "good.md")
	os.WriteFile(good, []byte("Never push to the kestrel repository without being asked. Kestrel pushes need a pull request. Ask first."), 0o644)
	out, code = runCmd(t, "memory", "replay", "--diff", "--note", "kestrel-rule.md", "--with", good, "--brief")
	if code != 0 || strings.Contains(out, "HOLD") {
		t.Fatalf("a harmless rewrite must pass: %d\n%s", code, out)
	}
}
