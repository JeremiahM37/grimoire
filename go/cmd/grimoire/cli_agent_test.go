package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// agentHome is a throwaway HOME with a fake grimoire-mcp beside PATH lookups,
// so nothing real is ever read or written.
func agentHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "grimoire-mcp"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return m
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, "*.grimoire-backup-*"))
	return m
}

func TestAgentInstallMergesClaudeCodeIdempotentlyAndUninstalls(t *testing.T) {
	home := agentHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo mine"}]}],` +
		`"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard.sh"}]}]},"permissions":{"allow":["Bash(ls)"]}}`
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(claudeJSON, []byte(`{"numStartups":7,"mcpServers":{"other":{"command":"x"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo", "--dry-run"}); code != 0 {
		t.Fatalf("dry run = %d", code)
	}
	if got, _ := os.ReadFile(settings); string(got) != mine {
		t.Fatal("--dry-run wrote to the settings file")
	}
	if _, err := os.Stat(filepath.Join(home, ".grimoire")); err == nil {
		t.Fatal("--dry-run wrote the hook script")
	}

	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo"}); code != 0 {
		t.Fatalf("install = %d", code)
	}
	root := readJSON(t, settings)
	if root["theme"] != "dark" || root["permissions"] == nil {
		t.Fatalf("unrelated settings lost: %v", root)
	}
	hooks := root["hooks"].(map[string]any)
	for _, ev := range []string{"SessionStart", "PostToolUse", "Stop", "SessionEnd"} {
		if hooks[ev] == nil {
			t.Errorf("missing %s hook", ev)
		}
	}
	if hooks["UserPromptSubmit"] != nil {
		t.Error("recall is opt-in")
	}
	stop := hooks["Stop"].([]any)
	if len(stop) != 2 || !strings.Contains(string(mustJSON(stop[0])), "echo mine") {
		t.Fatalf("a person's Stop hook must stay first and intact: %v", stop)
	}
	if hooks["PreToolUse"] == nil {
		t.Error("another event's hooks were lost")
	}
	cmdline := string(mustJSON(hooks["SessionStart"]))
	for _, want := range []string{"GRIMOIRE_BANK=coding-agent:demo", "GRIMOIRE_BANK_CONTEXT=1", "GRIMOIRE_BANK_HARNESS=claude-code",
		filepath.Join(home, ".grimoire", "hooks", "grimoire_bank_session.py")} {
		if !strings.Contains(cmdline, want) {
			t.Errorf("SessionStart command lacks %q: %s", want, cmdline)
		}
	}
	if strings.Contains(cmdline, "TOKEN") {
		t.Error("a token must never be written")
	}
	if len(backups(t, filepath.Dir(settings))) != 1 {
		t.Errorf("want one backup of settings.json: %v", backups(t, filepath.Dir(settings)))
	}
	if script, err := os.ReadFile(filepath.Join(home, ".grimoire", "hooks", "grimoire_bank_session.py")); err != nil || !strings.Contains(string(script), "GRIMOIRE_BANK_SESSIONS") {
		t.Errorf("hook script not installed: %v", err)
	}
	servers := readJSON(t, claudeJSON)["mcpServers"].(map[string]any)
	if servers["other"] == nil || servers["grimoire"] == nil || readJSON(t, claudeJSON)["numStartups"] == nil {
		t.Fatalf("mcp merge: %v", servers)
	}
	env := servers["grimoire"].(map[string]any)["env"].(map[string]any)
	if env["GRIMOIRE_BANK"] != "coding-agent:demo" || env["GRIMOIRE_AGENT_NAME"] != "claude-code" {
		t.Errorf("mcp env = %v", env)
	}

	// A second run changes nothing and makes no new backup.
	first, _ := os.ReadFile(settings)
	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo"}); code != 0 {
		t.Fatal("second install failed")
	}
	if again, _ := os.ReadFile(settings); string(again) != string(first) {
		t.Error("install is not idempotent")
	}
	if len(backups(t, filepath.Dir(settings))) != 1 || len(backups(t, home)) != 1 {
		t.Errorf("an unchanged install must not back up again: %v %v", backups(t, filepath.Dir(settings)), backups(t, home))
	}

	// Uninstall restores the shape: only ours goes.
	if code := cmdAgent([]string{"uninstall", "--claude-code"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	root = readJSON(t, settings)
	hooks = root["hooks"].(map[string]any)
	if len(hooks) != 2 || len(hooks["Stop"].([]any)) != 1 || hooks["SessionStart"] != nil || root["theme"] != "dark" {
		t.Fatalf("after uninstall: %v", root)
	}
	servers = readJSON(t, claudeJSON)["mcpServers"].(map[string]any)
	if servers["grimoire"] != nil || servers["other"] == nil {
		t.Fatalf("mcp after uninstall: %v", servers)
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

func TestAgentInstallCodexWritesAManagedTOMLBlock(t *testing.T) {
	home := agentHome(t)
	config := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "model = \"gpt\"\n\n[mcp_servers.other]\ncommand = \"x\"\n"
	if err := os.WriteFile(config, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := cmdAgent([]string{"install", "--codex"}); code != 0 {
		t.Fatal("install failed")
	}
	text, _ := os.ReadFile(config)
	if !strings.HasPrefix(string(text), original) || !strings.Contains(string(text), "[mcp_servers.grimoire]") ||
		!strings.Contains(string(text), `GRIMOIRE_AGENT_NAME = "codex"`) {
		t.Fatalf("config.toml:\n%s", text)
	}
	hooks := readJSON(t, filepath.Join(home, ".codex", "hooks.json"))["hooks"].(map[string]any)
	if hooks["Stop"] == nil || hooks["SessionStart"] == nil || hooks["SessionEnd"] != nil {
		t.Fatalf("codex hooks = %v", hooks)
	}
	if !strings.Contains(string(mustJSON(hooks["Stop"])), "GRIMOIRE_BANK_HARNESS=codex") {
		t.Error("the codex hook must say which harness it is")
	}
	if code := cmdAgent([]string{"install", "--codex"}); code != 0 {
		t.Fatal("second install failed")
	}
	if again, _ := os.ReadFile(config); string(again) != string(text) {
		t.Error("toml install is not idempotent")
	}
	if code := cmdAgent([]string{"uninstall", "--codex"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	if after, _ := os.ReadFile(config); string(after) != original {
		t.Fatalf("config.toml after uninstall:\n%q\nwant\n%q", after, original)
	}
}

func TestAgentInstallNeverOverwritesAUsersOwnServerOrBrokenFile(t *testing.T) {
	home := agentHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"mcpServers":{"grimoire":{"command":"/my/own/thing"}}}`
	claudeJSON := filepath.Join(home, ".claude.json")
	_ = os.WriteFile(claudeJSON, []byte(mine), 0o600)
	if code := cmdAgent([]string{"install", "--claude-code"}); code != 0 {
		t.Fatal("install failed")
	}
	if got, _ := os.ReadFile(claudeJSON); string(got) != mine {
		t.Errorf("a user's own grimoire entry was touched: %s", got)
	}
	// A settings file that is not JSON is refused, not replaced.
	settings := filepath.Join(home, ".claude", "settings.json")
	_ = os.WriteFile(settings, []byte("not json {"), 0o600)
	if code := cmdAgent([]string{"install", "--claude-code"}); code == 0 {
		t.Error("a broken settings file must fail the install")
	}
	if got, _ := os.ReadFile(settings); string(got) != "not json {" {
		t.Error("a broken settings file was overwritten")
	}
}

func TestAgentCommandNeedsAnAgentAndAValidBank(t *testing.T) {
	agentHome(t) // empty HOME: no ~/.claude, no ~/.codex
	if code := cmdAgent([]string{"install"}); code == 0 {
		t.Error("no agent found must be an error")
	}
	_ = os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".claude"), 0o755)
	if code := cmdAgent([]string{"install", "--bank", "Bad Bank!"}); code == 0 {
		t.Error("an invalid bank id must be refused")
	}
	if code := cmdAgent([]string{"nonsense"}); code == 0 {
		t.Error("unknown subcommand")
	}
}

func TestAgentInstallFilesFlagAddsReadHookAndKeepsAPersonsPreToolUse(t *testing.T) {
	home := agentHome(t)
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settings), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := `{"hooks":{"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"guard.sh"}]}]}}`
	if err := os.WriteFile(settings, []byte(mine), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo"}); code != 0 {
		t.Fatalf("install = %d", code)
	}
	pre := readJSON(t, settings)["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 || strings.Contains(string(mustJSON(pre)), "GRIMOIRE_BANK_FILES") {
		t.Fatalf("file memory must be off by default: %v", pre)
	}
	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo", "--files"}); code != 0 {
		t.Fatalf("install --files = %d", code)
	}
	pre = readJSON(t, settings)["hooks"].(map[string]any)["PreToolUse"].([]any)
	got := string(mustJSON(pre))
	if len(pre) != 2 || !strings.Contains(got, "guard.sh") || !strings.Contains(got, "GRIMOIRE_BANK_FILES=1") ||
		!strings.Contains(got, `"matcher":"Read"`) {
		t.Fatalf("PreToolUse = %s", got)
	}
	if code := cmdAgent([]string{"install", "--claude-code", "--bank", "coding-agent:demo"}); code != 0 {
		t.Fatal("reinstall without --files")
	}
	pre = readJSON(t, settings)["hooks"].(map[string]any)["PreToolUse"].([]any)
	if len(pre) != 1 || !strings.Contains(string(mustJSON(pre)), "guard.sh") {
		t.Fatalf("dropping --files must remove only ours: %v", pre)
	}
}

// A fake `codex app-server` that answers hooks/list the way Codex 0.157 does.
// Codex runs a hook only once config.toml holds its trusted_hash, so the
// installer must copy Codex's own hash for every hook it wrote, and no other.
func TestAgentInstallCodexRecordsTrustForItsOwnHooksOnly(t *testing.T) {
	home := agentHome(t)
	bin := t.TempDir()
	fake := `#!/bin/sh
while read -r line; do
  case "$line" in *hooks/list*)
    h="$CODEX_HOME/hooks.json"
    echo "{\"id\":2,\"result\":{\"data\":[{\"cwd\":\"/\",\"hooks\":[" \
      "{\"key\":\"$h:stop:0:0\",\"command\":\"python3 grimoire_bank_session.py\",\"sourcePath\":\"$h\",\"currentHash\":\"sha256:aaa\"}," \
      "{\"key\":\"$h:stop:1:0\",\"command\":\"someone-elses-hook\",\"sourcePath\":\"$h\",\"currentHash\":\"sha256:bbb\"}]}]}}" | tr -d '\n'; echo; exit 0;;
  esac
done
`
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(config), 0o755); err != nil {
		t.Fatal(err)
	}
	original := "model = \"gpt\"\n"
	if err := os.WriteFile(config, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := cmdAgent([]string{"install", "--codex"}); code != 0 {
		t.Fatal("install failed")
	}
	text, _ := os.ReadFile(config)
	if !strings.Contains(string(text), `trusted_hash = "sha256:aaa"`) || strings.Contains(string(text), "sha256:bbb") {
		t.Fatalf("trust block wrong:\n%s", text)
	}
	if code := cmdAgent([]string{"install", "--codex"}); code != 0 {
		t.Fatal("second install failed")
	}
	if again, _ := os.ReadFile(config); string(again) != string(text) {
		t.Errorf("trust block is not idempotent:\n%s\n---\n%s", text, again)
	}
	for _, b := range backups(t, filepath.Dir(config)) {
		if strings.Contains(b, "config.toml") {
			// The earliest copy is the user's own file, even with several edits in one second.
			if kept, _ := os.ReadFile(b); string(kept) != original {
				t.Errorf("backup %s = %q, want the original", b, kept)
			}
		}
	}
	if code := cmdAgent([]string{"uninstall", "--codex"}); code != 0 {
		t.Fatal("uninstall failed")
	}
	if after, _ := os.ReadFile(config); string(after) != original {
		t.Fatalf("config.toml after uninstall:\n%q", after)
	}
}
