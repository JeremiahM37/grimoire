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
