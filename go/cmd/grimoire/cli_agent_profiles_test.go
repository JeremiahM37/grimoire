package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentInstallMemoryHookForCodexIsMergedIdempotentAndRemovable(t *testing.T) {
	home := agentHome(t)
	hooksFile := filepath.Join(home, ".codex", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(hooksFile), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"hooks":{"PreToolUse":[{"hooks":[{"type":"command","command":"theirs.sh"}]}]}}`
	if err := os.WriteFile(hooksFile, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := cmdAgent([]string{"install", "--codex", "--memory", "--no-mcp", "--dry-run"}); code != 0 {
		t.Fatalf("dry run = %d", code)
	}
	if got, _ := os.ReadFile(hooksFile); string(got) != mine {
		t.Fatal("--dry-run changed hooks.json")
	}
	if _, err := os.Stat(filepath.Join(home, ".grimoire")); err == nil {
		t.Fatal("--dry-run wrote under ~/.grimoire")
	}

	if code := cmdAgent([]string{"install", "--codex", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("install failed")
	}
	hooks := readJSON(t, hooksFile)["hooks"].(map[string]any)
	pre := string(mustJSON(hooks["PreToolUse"]))
	if !strings.Contains(pre, "theirs.sh") || !strings.Contains(pre, "grimoire_context.py") ||
		!strings.Contains(pre, `"matcher":"Bash|apply_patch"`) || !strings.Contains(pre, "--agent codex") {
		t.Fatalf("PreToolUse = %s", pre)
	}
	if !strings.Contains(string(mustJSON(hooks["UserPromptSubmit"])), "grimoire_context.py") ||
		strings.Contains(string(mustJSON(hooks["UserPromptSubmit"])), "BANK") {
		t.Fatalf("UserPromptSubmit = %v", hooks["UserPromptSubmit"])
	}
	// The bank hooks and the memory hook share SessionStart as two groups.
	if start := hooks["SessionStart"].([]any); len(start) != 2 {
		t.Fatalf("SessionStart = %v", start)
	}
	for _, f := range []string{
		filepath.Join(home, ".grimoire", "hooks", "grimoire_context.py"),
		filepath.Join(home, ".grimoire", "agents", "codex.json"),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	first, _ := os.ReadFile(hooksFile)
	if code := cmdAgent([]string{"install", "--codex", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("second install failed")
	}
	if again, _ := os.ReadFile(hooksFile); string(again) != string(first) {
		t.Error("install --memory is not idempotent")
	}

	// Installing without --memory later drops only the memory hook.
	if code := cmdAgent([]string{"install", "--codex", "--no-mcp"}); code != 0 {
		t.Fatal("reinstall failed")
	}
	hooks = readJSON(t, hooksFile)["hooks"].(map[string]any)
	if strings.Contains(string(mustJSON(hooks)), "grimoire_context.py") || !strings.Contains(string(mustJSON(hooks)), "theirs.sh") ||
		hooks["Stop"] == nil {
		t.Fatalf("hooks after dropping --memory: %v", hooks)
	}

	if code := cmdAgent([]string{"install", "--codex", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("install failed")
	}
	if code := cmdAgent([]string{"uninstall", "--codex"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	after := readJSON(t, hooksFile)["hooks"].(map[string]any)
	if len(after) != 1 || !strings.Contains(string(mustJSON(after)), "theirs.sh") {
		t.Fatalf("after uninstall: %v", after)
	}
	if _, err := os.Stat(filepath.Join(home, ".grimoire", "agents", "codex.json")); err == nil {
		t.Error("the resolved profile should go with the hooks")
	}
}

func TestAgentInstallClaudeCodeMemoryHookUsesTheLiveShape(t *testing.T) {
	home := agentHome(t)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	if code := cmdAgent([]string{"install", "--claude-code", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("install failed")
	}
	hooks := readJSON(t, filepath.Join(home, ".claude", "settings.json"))["hooks"].(map[string]any)
	pre := string(mustJSON(hooks["PreToolUse"]))
	if !strings.Contains(pre, "Agent|Bash|Edit|MultiEdit|NotebookEdit|Task|Write") && !strings.Contains(pre, "Bash") {
		t.Fatalf("PreToolUse = %s", pre)
	}
	for _, ev := range []string{"SessionStart", "UserPromptSubmit", "PreToolUse"} {
		if !strings.Contains(string(mustJSON(hooks[ev])), "GRIMOIRE_CONTEXT_MODE=all") {
			t.Errorf("%s lacks the memory hook", ev)
		}
	}
}

func TestANewAgentIsOneProfileAndNoCode(t *testing.T) {
	home := agentHome(t)
	if code := cmdAgent([]string{"new", "zed"}); code != 0 {
		t.Fatal("new failed")
	}
	if code := cmdAgent([]string{"new", "zed"}); code == 0 {
		t.Error("new must not overwrite")
	}
	path := filepath.Join(home, ".config", "grimoire", "agents", "zed.json")
	if code := cmdAgent([]string{"install", "--agent", "zed", "--memory"}); code == 0 {
		t.Error("a starter with no hooks.file cannot be installed")
	}
	profile := `{"hooks":{"file":"~/.zed/hooks.json","events":{"prompt":"user_message","pre_action":"before_command"}},
	"event_fields":{"event":"type","prompt":"message.text","tool":"command.kind","tool_input":"command.args","session":"conversation"},
	"actions":{"shell":"cmd"},"output":"plain-stdout"}`
	if err := os.WriteFile(path, []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdAgent([]string{"profiles"}); code != 0 {
		t.Fatal("profiles failed")
	}
	if code := cmdAgent([]string{"install", "--agent", "zed", "--memory"}); code != 0 {
		t.Fatal("install failed")
	}
	hooks := readJSON(t, filepath.Join(home, ".zed", "hooks.json"))["hooks"].(map[string]any)
	if len(hooks) != 2 || !strings.Contains(string(mustJSON(hooks["before_command"])), `"matcher":"shell"`) ||
		!strings.Contains(string(mustJSON(hooks["user_message"])), "--agent zed") {
		t.Fatalf("zed hooks = %v", hooks)
	}
	if code := cmdAgent([]string{"status", "--agent", "zed"}); code != 0 {
		t.Fatal("status failed")
	}
	if code := cmdAgent([]string{"uninstall", "--agent", "zed"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	if code := cmdAgent([]string{"install", "--agent", "nope"}); code == 0 {
		t.Error("unknown profile")
	}
}

// The installed profile and the installed script, end to end: the hook that
// `install --memory` wrote, run the way the agent runs it, asking a stub server.
func TestInstalledMemoryHookInjectsForANewAgent(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3")
	}
	home := agentHome(t)
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{"context":"- use uv, not pip (m:3e99)","keys":["` + strings.Repeat("c", 32) + `"]}`))
	}))
	defer srv.Close()
	dir := filepath.Join(home, ".config", "grimoire", "agents")
	_ = os.MkdirAll(dir, 0o755)
	profile := `{"hooks":{"file":"~/.zed/hooks.json","events":{"prompt":"user_message"}},
	"event_fields":{"event":"type","prompt":"message.text","session":"conversation"},"output":"plain-stdout"}`
	_ = os.WriteFile(filepath.Join(dir, "zed.json"), []byte(profile), 0o644)
	if code := cmdAgent([]string{"install", "--agent", "zed", "--memory", "--no-mcp", "--url", srv.URL}); code != 0 {
		t.Fatal("install failed")
	}
	cmd := exec.Command(python, filepath.Join(home, ".grimoire", "hooks", "grimoire_context.py"), "--agent", "zed")
	cmd.Env = append(os.Environ(), "HOME="+home, "GRIMOIRE_CONTEXT_MODE=all", "GRIMOIRE_URL="+srv.URL,
		"GRIMOIRE_CONTEXT_STATE_DIR="+t.TempDir())
	cmd.Stdin = strings.NewReader(`{"type":"user_message","conversation":"c1","message":{"text":"how do I install kestrel deps"}}`)
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(out)) != "- use uv, not pip (m:3e99)" || !strings.Contains(asked, "kestrel") {
		t.Errorf("hook printed %q after asking %q", out, asked)
	}
}

func TestContextCommandPrintsTheBlock(t *testing.T) {
	agentHome(t)
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = map[string]string{"q": r.URL.Query().Get("q"), "stage": r.URL.Query().Get("stage"), "min_rel": r.URL.Query().Get("min_rel")}
		_, _ = w.Write([]byte(`{"context":"- remember this"}`))
	}))
	defer srv.Close()
	t.Setenv("GRIMOIRE_URL", srv.URL)
	if code := cmdContext([]string{"--event", "action", "--tool", "Bash", "--text", "pip install x"}); code != 0 {
		t.Fatal("context failed")
	}
	if got["q"] != "Bash pip install x" || got["stage"] != "action" || got["min_rel"] != "0.7" {
		t.Errorf("asked %v", got)
	}
	if code := cmdContext([]string{"--event", "prompt", "--agent", "codex", "--text", "hello there"}); code != 0 {
		t.Fatal("context failed")
	}
	if got["stage"] != "" || got["min_rel"] != "0.5" {
		t.Errorf("prompt asked %v", got)
	}
	if code := cmdContext([]string{"--event", "bogus", "--text", "x"}); code == 0 {
		t.Error("bad event")
	}
}

func TestAgentInstallMemoryAlsoInstallsTheOutcomeHook(t *testing.T) {
	home := agentHome(t)
	_ = os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	settings := filepath.Join(home, ".claude", "settings.json")
	if code := cmdAgent([]string{"install", "--claude-code", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("install failed")
	}
	if _, err := os.Stat(filepath.Join(home, ".grimoire", "hooks", "grimoire_outcome.py")); err != nil {
		t.Fatalf("outcome script not written: %v", err)
	}
	hooks := readJSON(t, settings)["hooks"].(map[string]any)
	for _, ev := range []string{"PostToolUse", "Stop"} {
		got := string(mustJSON(hooks[ev]))
		if !strings.Contains(got, "grimoire_outcome.py") {
			t.Errorf("%s lacks the outcome hook: %s", ev, got)
		}
	}
	if !strings.Contains(string(mustJSON(hooks["PostToolUse"])), "--event post_action") ||
		!strings.Contains(string(mustJSON(hooks["Stop"])), "--event stop") {
		t.Errorf("outcome commands do not name their event: %v", hooks)
	}
	if strings.Contains(string(mustJSON(hooks["PreToolUse"])), "grimoire_outcome.py") {
		t.Error("outcome hook must not sit on PreToolUse")
	}
	first, _ := os.ReadFile(settings)
	if code := cmdAgent([]string{"install", "--claude-code", "--memory", "--no-mcp"}); code != 0 {
		t.Fatal("second install failed")
	}
	if again, _ := os.ReadFile(settings); string(again) != string(first) {
		t.Error("install is not idempotent with the outcome hook")
	}
	// Without --memory the outcome hook goes; uninstall removes everything.
	if code := cmdAgent([]string{"install", "--claude-code", "--no-mcp"}); code != 0 {
		t.Fatal("reinstall failed")
	}
	if strings.Contains(string(mustJSON(readJSON(t, settings)["hooks"])), "grimoire_outcome.py") {
		t.Error("outcome hook survived a reinstall without --memory")
	}
	_ = cmdAgent([]string{"install", "--claude-code", "--memory", "--no-mcp"})
	if code := cmdAgent([]string{"uninstall", "--claude-code"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	if strings.Contains(string(mustJSON(readJSON(t, settings))), "grimoire_outcome.py") {
		t.Error("uninstall left the outcome hook")
	}
}
