package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const procNote = "---\nname: Deploy kestrel\ndescription: how to deploy kestrel\nmetadata:\n  kind: procedure\n  cues: deploying kestrel\n---\n1. build:\n```bash\nmake build\n```\n2. ship\n3. check\n"

func TestAgentInstallLinkMemoryFollowsTheProfile(t *testing.T) {
	home := agentHome(t)
	vaultDir(t)
	store := filepath.Join(t.TempDir(), "store")
	os.MkdirAll(store, 0o755)
	os.WriteFile(filepath.Join(store, "feedback_a.md"), []byte("---\nname: a\ndescription: never do a\nmetadata:\n  type: feedback\n---\nbody"), 0o644)

	// Claude Code: one project already linked-style (empty), one that is the store itself.
	claudeMem := filepath.Join(home, ".claude", "projects", "-w-proj", "memory")
	os.MkdirAll(claudeMem, 0o755) // empty real dir: becomes a symlink
	os.MkdirAll(filepath.Join(home, ".claude", "projects"), 0o755)
	os.Symlink(store, filepath.Join(home, ".claude", "projects", "-home-admin"+"-memory-link"))
	if err := os.MkdirAll(filepath.Join(home, ".claude", "projects", "-w-linked"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.Symlink(store, filepath.Join(home, ".claude", "projects", "-w-linked", "memory"))
	// Codex: an AGENTS.md of the user's own.
	agentsMD := filepath.Join(home, ".codex", "AGENTS.md")
	os.MkdirAll(filepath.Dir(agentsMD), 0o755)
	os.WriteFile(agentsMD, []byte("# my instructions\n"), 0o644)

	if code := cmdAgent([]string{"install", "--claude-code", "--codex", "--no-mcp", "--link-memory", "--dir", store, "--dry-run"}); code != 0 {
		t.Fatalf("dry run = %d", code)
	}
	if fi, _ := os.Lstat(claudeMem); fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("dry run linked")
	}
	if b, _ := os.ReadFile(agentsMD); string(b) != "# my instructions\n" {
		t.Fatal("dry run wrote the file")
	}
	if code := cmdAgent([]string{"install", "--claude-code", "--codex", "--no-mcp", "--link-memory", "--dir", store}); code != 0 {
		t.Fatalf("install = %d", code)
	}
	if fi, err := os.Lstat(claudeMem); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("claude memory not linked: %v", err)
	}
	b, _ := os.ReadFile(agentsMD)
	if !strings.HasPrefix(string(b), "# my instructions\n") || !strings.Contains(string(b), "never do a") {
		t.Fatalf("AGENTS.md:\n%s", b)
	}
	// The status view shows it, and a second run changes nothing.
	out := captureStdout(t, func() { cmdAgent([]string{"status", "--claude-code", "--codex"}) })
	if !strings.Contains(out, "memory link:") || !strings.Contains(out, "linked") {
		t.Errorf("status:\n%s", out)
	}
	if code := cmdAgent([]string{"install", "--claude-code", "--codex", "--no-mcp", "--link-memory", "--dir", store}); code != 0 {
		t.Fatalf("second install = %d", code)
	}
	b2, _ := os.ReadFile(agentsMD)
	if string(b2) != string(b) {
		t.Error("second run rewrote the managed block")
	}
	// The link is recorded for `grimoire memory status`.
	st, _ := runCmd(t, "memory", "status", "--dir", store)
	if !strings.Contains(st, "claude-code") || !strings.Contains(st, "codex") {
		t.Errorf("memory status:\n%s", st)
	}
}

func TestSkillsExportMineAcceptAndImpact(t *testing.T) {
	home := agentHome(t)
	vaultDir(t)
	store := filepath.Join(t.TempDir(), "store")
	os.MkdirAll(store, 0o755)
	os.WriteFile(filepath.Join(store, "reference_deploy.md"), []byte(procNote), 0o644)

	// A new agent with a profile: skills dir, transcripts as generic JSONL.
	cfg := filepath.Join(home, ".config", "grimoire", "agents")
	os.MkdirAll(cfg, 0o755)
	logs := filepath.Join(home, "logs")
	os.MkdirAll(logs, 0o755)
	profile := `{"hooks":{"file":""},"skills_dir":"~/skills","transcripts":{"glob":"~/logs/*.jsonl","format":"generic-jsonl",` +
		`"map":{"session":"s","time":"t","role":"r","text":"x","tool":"tool","tool_target":"cmd","tool_error":"err"}}}`
	os.WriteFile(filepath.Join(cfg, "myagent.json"), []byte(profile), 0o644)

	// export
	out, code := runCmd(t, "skills", "export", "--agent", "myagent", "--dir", store, "--dry-run")
	if code != 0 || !strings.Contains(out, "created") {
		t.Fatalf("dry run = %d:\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(home, "skills")); err == nil {
		t.Fatal("dry run created the skills dir")
	}
	if out, code = runCmd(t, "skills", "export", "--agent", "myagent", "--dir", store); code != 0 {
		t.Fatalf("export = %d:\n%s", code, out)
	}
	skill := filepath.Join(home, "skills", "deploy-kestrel", "SKILL.md")
	b, err := os.ReadFile(skill)
	if err != nil || !strings.Contains(string(b), "Use when deploying kestrel") {
		t.Fatalf("%v\n%s", err, b)
	}
	if out, _ = runCmd(t, "skills", "export", "--agent", "myagent", "--dir", store); !strings.Contains(out, "1 unchanged") {
		t.Errorf("second export:\n%s", out)
	}

	// mine: the same four-command run in four sessions.
	var lines []string
	base := time.Now().AddDate(0, 0, -10)
	for s := 0; s < 4; s++ {
		for i, cmd := range []string{"git status", "go test ./...", "go build ./cmd/x", "git push origin main"} {
			lines = append(lines, fmt.Sprintf(`{"s":"s%d","t":%d,"tool":"bash","cmd":%q,"err":0}`, s, base.AddDate(0, 0, s).Add(time.Duration(i)*time.Minute).Unix(), cmd))
		}
	}
	os.WriteFile(filepath.Join(logs, "a.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	out, code = runCmd(t, "skills", "mine", "--agent", "myagent", "--since", "30d")
	if code != 0 || !strings.Contains(out, "1 candidate") || !strings.Contains(out, "review queue") {
		t.Fatalf("mine = %d:\n%s", code, out)
	}
	queue := filepath.Join(os.Getenv("GRIMOIRE_VAULT"), ".grimoire", "skill-candidates.json")
	raw, err := os.ReadFile(queue)
	if err != nil {
		t.Fatal(err)
	}
	var cands []struct{ ID string }
	if json.Unmarshal(raw, &cands) != nil || len(cands) != 1 {
		t.Fatalf("queue %s", raw)
	}
	if entries, _ := filepath.Glob(filepath.Join(store, "procedure_*")); len(entries) != 0 {
		t.Fatal("mine must not add memories")
	}
	if out, code = runCmd(t, "skills", "accept", cands[0].ID, "--dir", store); code != 0 {
		t.Fatalf("accept = %d:\n%s", code, out)
	}
	if entries, _ := filepath.Glob(filepath.Join(store, "procedure_*")); len(entries) != 1 {
		t.Fatalf("accept wrote %v", entries)
	}
	if _, code = runCmd(t, "skills", "accept", cands[0].ID, "--dir", store); code == 0 {
		t.Error("accepting twice must not overwrite")
	}

	// remove: only what export wrote.
	os.MkdirAll(filepath.Join(home, "skills", "handmade"), 0o755)
	os.WriteFile(filepath.Join(home, "skills", "handmade", "SKILL.md"), []byte("mine"), 0o644)
	if out, code = runCmd(t, "skills", "export", "--agent", "myagent", "--remove"); code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("remove = %d:\n%s", code, out)
	}
	if _, err := os.Stat(skill); err == nil {
		t.Error("skill still there")
	}
	if _, err := os.Stat(filepath.Join(home, "skills", "handmade", "SKILL.md")); err != nil {
		t.Error("remove touched a skill it did not write")
	}

	// impact runs end to end (too little data to compare, and says so).
	t.Setenv("GRIMOIRE_MEMORY_CANONICAL_DIR", store)
	out, code = runCmd(t, "memory", "impact", "--since", "60d")
	if code != 0 || !strings.Contains(out, "Correlation only") {
		t.Fatalf("impact = %d:\n%s", code, out)
	}
	out, code = runCmd(t, "memory", "impact", "--retells", "--since", "60d")
	if code != 0 || !strings.Contains(out, "Re-tells per 100 user prompts") || !strings.Contains(out, "strict") {
		t.Fatalf("retells = %d:\n%s", code, out)
	}
}
