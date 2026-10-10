package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
)

// hookRunner runs the real hook scripts as an agent would: JSON on stdin, the
// server over HTTP, state in a private directory.
type hookRunner struct {
	t       *testing.T
	dir     string
	baseEnv []string
}

func newHookRunner(t *testing.T, url string) *hookRunner {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &hookRunner{t: t, dir: dir, baseEnv: []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + dir, // nothing under the real home
		"GRIMOIRE_URL=" + url, "GRIMOIRE_CONTEXT_DEBUG=1", "GRIMOIRE_CONTEXT_MODE=all", "GRIMOIRE_CONTEXT_STATE_DIR=" + filepath.Join(dir, "state"),
		"GRIMOIRE_AGENT_DIR=" + filepath.Join(dir, "agents"), "GRIMOIRE_ACTION_MIN_REL=0.3", "GRIMOIRE_EDIT_MIN_REL=0.3",
	}}
}

func (h *hookRunner) run(script, agent, event string, payload map[string]any) string {
	h.t.Helper()
	raw, _ := json.Marshal(payload)
	args := []string{filepath.Join("..", "..", "..", "clients", "hooks", script)}
	if agent != "" {
		args = append(args, "--agent", agent)
	}
	if event != "" {
		args = append(args, "--event", event)
	}
	cmd := exec.Command("python3", args...)
	cmd.Env = h.baseEnv
	cmd.Stdin = bytes.NewReader(raw)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		h.t.Fatalf("%s %s: %v\n%s", script, event, err, stderr.String())
	}
	if stderr.Len() > 0 {
		h.t.Logf("%s %s stderr: %s", script, event, stderr.String())
	}
	return out.String()
}

func (h *hookRunner) context(agent, event string, p map[string]any) string {
	return h.run("grimoire_context.py", agent, event, p)
}
func (h *hookRunner) outcome(agent, event string, p map[string]any) string {
	return h.run("grimoire_outcome.py", agent, event, p)
}

// TestTraceEndToEndThroughTheHooks plays two synthetic sessions through the
// real hook scripts against a real server: one in Claude Code's payload shape,
// one for an agent described only by a profile file. It checks the whole chain:
// exposure -> uptake -> influence (a linked action, and "changed after the
// reminder") -> outcome codes -> the card, and that no text reaches the store.
func TestTraceEndToEndThroughTheHooks(t *testing.T) {
	old := adherence.PromptMergeWindow
	adherence.PromptMergeWindow = 0
	t.Cleanup(func() { adherence.PromptMergeWindow = old })
	s, handler := testServer(t)
	srv := httptest.NewServer(handler)
	defer srv.Close()
	fillStore(t, handler, 60)
	zebra := remember(t, handler, map[string]any{"topic": "zebra", "text": zebraFact})
	otter := remember(t, handler, map[string]any{"topic": "otters", "text": "Otter enclosure cleaning happens on Tuesdays, see /srv/otters/schedule-marmot.csv"})
	h := newHookRunner(t, srv.URL)

	// ---- Claude Code's payload shape --------------------------------------
	const claude = "claude-session-0001"
	h.context("", "prompt", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": claude, "cwd": "/w",
		"prompt": "zebra feeding schedule needs checking"})
	out := h.context("", "action", map[string]any{"hook_event_name": "PreToolUse", "session_id": claude, "cwd": "/w",
		"tool_name": "Bash", "tool_use_id": "toolu_A1", "tool_input": map[string]any{"command": "zebra feeding schedule dawn"}})
	if !strings.Contains(out, "m:") {
		t.Fatalf("no action-stage injection: %q", out)
	}
	// The agent does something else instead: edits the file the memory names.
	h.outcome("", "post_action", map[string]any{"hook_event_name": "PostToolUse", "session_id": claude, "tool_name": "Bash",
		"tool_use_id": "toolu_A1", "tool_input": map[string]any{"command": "vi /srv/zebra/feeder-quokka.yaml"},
		"tool_response": map[string]any{"stdout": "SECRET-OUTPUT", "interrupted": false}})
	// Tests fail, then it re-runs them three times.
	for i := 0; i < 3; i++ {
		h.outcome("", "post_action_failure", map[string]any{"hook_event_name": "PostToolUseFailure", "session_id": claude,
			"tool_name": "Bash", "tool_use_id": "toolu_B" + string(rune('1'+i)), "tool_input": map[string]any{"command": "go test ./..."},
			"error": "Exit code 1\n--- FAIL: TestFeeder (0.00s)\nFAIL\tzebra\t0.1s"})
	}
	h.outcome("", "stop", map[string]any{"hook_event_name": "Stop", "session_id": claude})
	time.Sleep(1100 * time.Millisecond)
	// The user's next prompt corrects the agent.
	h.outcome("", "prompt", map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": claude,
		"prompt": "no, that's not what I asked"})

	zt := "fact:" + zebra["id"].(string)
	c := getTrace(t, handler, zt).Card
	if c.Exposures < 1 || c.Evidence["fp"] != 1 || c.Evidence["changed"] != 1 || c.Reminders.Changed != 1 {
		t.Fatalf("zebra card: %+v", c)
	}
	if c.Uptake.K < 1 || c.Linked.Actions != 1 || c.Benefit.Label != "associated" {
		t.Fatalf("zebra uptake/influence: %+v", c)
	}
	// The outcome codes landed on the right calls, as codes.
	st := s.adh()
	sid := sha256.Sum256([]byte("session\x00" + claude))
	acts, err := st.Actions(hex.EncodeToString(sid[:])[:32])
	if err != nil || len(acts) != 4 {
		t.Fatalf("actions %v %v", acts, err)
	}
	if acts[0].Failed != 0 || acts[0].TestsFail != -1 {
		t.Fatalf("first call: %+v", acts[0])
	}
	for i, a := range acts[1:] {
		if a.Failed != 1 || a.TestsFail != 1 || a.TestsPass != 0 {
			t.Fatalf("failing test call %d: %+v", i, a)
		}
	}
	if acts[3].Thrash != 3 || acts[2].Thrash != 0 {
		t.Fatalf("thrash: %+v", acts)
	}
	for _, a := range acts {
		if !a.Correction {
			t.Fatalf("the next prompt corrected the turn: %+v", a)
		}
	}
	if sum := do(t, handler, "GET", "/api/memory/trace/summary", nil).Body.String(); !strings.Contains(sum, `"associated"`) {
		t.Fatalf("summary: %s", sum)
	}

	// ---- an agent known only by its profile --------------------------------
	profile := `{"name":"pi","hooks":{"events":{"prompt":"user_message","pre_action":"tool_start","post_action":"tool_finished","stop":"turn_end"}},
	  "event_fields":{"event":"type","prompt":"message.text","tool":"call.name","tool_input":"call.args","session":"conversation",
	                  "tool_response":"call.result","tool_use_id":"call.id"},
	  "actions":{"shell":"cmd"},"delegation_tools":[],"output":"plain-stdout"}`
	if err := os.WriteFile(filepath.Join(h.dir, "agents", "pi.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	const pi = "pi-conversation-0001"
	h.context("pi", "", map[string]any{"type": "user_message", "conversation": pi, "message": map[string]any{"text": "otter enclosure cleaning tuesdays question"}})
	out = h.context("pi", "", map[string]any{"type": "tool_start", "conversation": pi,
		"call": map[string]any{"name": "shell", "id": "call_9", "args": map[string]any{"cmd": "otter enclosure cleaning tuesdays"}}})
	if !strings.Contains(out, "m:") {
		t.Fatalf("pi: no injection: %q", out)
	}
	h.outcome("pi", "", map[string]any{"type": "tool_finished", "conversation": pi,
		"call": map[string]any{"name": "shell", "id": "call_9", "args": map[string]any{"cmd": "otter enclosure cleaning tuesdays"},
			"result": map[string]any{"output": "ok  \tpkg\t0.1s", "exit_code": 0}}})
	h.outcome("pi", "", map[string]any{"type": "turn_end", "conversation": pi})
	ot := getTrace(t, handler, "fact:"+otter["id"].(string))
	// The pending action ran unchanged: recorded as such, with no influence link.
	if ot.Card.Reminders.Unchanged != 1 || ot.Card.Evidence["changed"] != 0 {
		t.Fatalf("pi card: %+v", ot.Card)
	}

	// No command, path, output or prompt text is in the store.
	st.Close()
	for _, f := range []string{"adherence.db", "adherence.db-wal"} {
		b, err := os.ReadFile(filepath.Join(s.Vault.Root, ".grimoire", f))
		if err != nil {
			continue
		}
		for _, secret := range []string{"SECRET-OUTPUT", "not what I asked", "feeder-quokka.yaml", "go test"} {
			if bytes.Contains(b, []byte(secret)) {
				t.Errorf("%s contains %q", f, secret)
			}
		}
	}
}
