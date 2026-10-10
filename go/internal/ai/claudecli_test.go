package ai

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type cliSettings map[string]string

func (s cliSettings) Get(k string) string { return s[k] }

// A stand-in `claude` that echoes its prompt and records its flags: proves the
// command is run with no tools, no MCP and no settings, from a scratch
// directory, and that its JSON reply is read back with token counts.
func TestClaudeCLIBackend(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "args")
	script := filepath.Join(dir, "claude")
	body := "#!/bin/sh\necho \"$@ cwd=$(pwd)\" > " + log + "\nIN=$(cat)\nprintf '{\"result\":\"echo: %s\",\"is_error\":false,\"total_cost_usd\":0.5,\"duration_ms\":7,\"usage\":{\"input_tokens\":3,\"cache_read_input_tokens\":4,\"output_tokens\":2}}' \"$IN\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	c := New(cliSettings{"llm": BackendClaudeCLI, "llm_model": "claude-sonnet-5-5", "claude_cli_path": script}, nil)
	if c.Backend() != BackendClaudeCLI || !c.Available() {
		t.Fatal("backend not selected")
	}
	comp, rep, err := c.CompleteCLI(context.Background(), "hello", CompleteOpts{System: "sys"})
	if err != nil || comp.Text != "echo: hello" || comp.Usage.Input != 7 || comp.Usage.Output != 2 || rep.CostUSD != 0.5 {
		t.Fatalf("%+v %+v %v", comp, rep, err)
	}
	raw, _ := os.ReadFile(log)
	for _, want := range []string{"--tools ", "--strict-mcp-config", "--setting-sources", "--max-turns 1", "--system-prompt sys", "--model claude-sonnet-5-5"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in %s", want, raw)
		}
	}
	if strings.Contains(string(raw), "cwd="+dir) {
		t.Error("ran from the working directory, not a scratch dir")
	}
	// Complete (the plain entry point) goes through the same backend.
	if out, err := c.Complete("again", ""); err != nil || out != "echo: again" {
		t.Fatalf("%q %v", out, err)
	}
}
