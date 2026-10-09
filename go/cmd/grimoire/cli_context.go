package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
)

// grimoire context --event prompt|action --text TEXT [--tool NAME] [--agent NAME]
//
// The memory block the hook would inject, printed. For wrappers and agents with
// no hook system: run it before a prompt or a command and paste or pipe the
// output. It asks the same endpoint and uses the same bars as
// clients/hooks/grimoire_context.py, but keeps no per-session state, so repeated
// calls repeat memories; pass --session to let the server pair re-tells.

const contextUsage = `usage: grimoire context --event prompt|action --text TEXT [--tool NAME]
                        [--agent NAME] [--session ID] [--budget BYTES] [--json]

  --event prompt   the memories that bear on a request (TEXT is the prompt)
  --event action   the memories that bear on a command or edit about to run
                   (TEXT is the command or path; --tool names it, e.g. Bash)
  --agent NAME     print in that agent's output format (claude-json | plain-stdout);
                   default is the bare text
  --json           print the server's reply as is

Prints nothing when no memory applies. Env: GRIMOIRE_URL, GRIMOIRE_AUTH_TOKEN.`

func init() {
	bankValued["--event"] = true
	bankValued["--tool"] = true
}

func cmdContext(args []string) int {
	f, err := parseBankFlags(args)
	if err != nil {
		return fail("%v", err)
	}
	text := f.str("--text", strings.Join(f.pos, " "))
	event := f.str("--event", "prompt")
	if f.on["--help"] || strings.TrimSpace(text) == "" || (event != "prompt" && event != "action") {
		fmt.Println(contextUsage)
		if f.on["--help"] {
			return 0
		}
		return 2
	}
	q := url.Values{"scope": {"all"}, "rank": {"hybrid"}}
	query := strings.TrimSpace(text)
	tool := f.str("--tool", "")
	if event == "action" {
		// The bars the hook uses for actions (docs/MEMORY_USE.md): commands at 0.7,
		// file edits at 0.8, because a path resembles every memory about its repo.
		if tool != "" {
			query = tool + " " + query
		}
		q.Set("stage", "action")
		q.Set("limit", "2")
		q.Set("max_bytes", "1200")
		q.Set("min_rel", "0.7")
		if tool != "" && !strings.EqualFold(tool, "bash") && !strings.EqualFold(tool, "shell") {
			q.Set("min_rel", "0.8")
		}
	} else {
		q.Set("limit", "5")
		q.Set("max_bytes", "2400")
		q.Set("min_rel", "0.5")
	}
	if len(query) > 2000 {
		query = query[:2000]
	}
	q.Set("q", query)
	if b := f.str("--budget", ""); b != "" {
		q.Set("max_bytes", b)
	}
	if s := f.str("--session", ""); s != "" {
		q.Set("session", s)
	}
	c := newBankClient(f)
	var out struct {
		Context string `json:"context"`
	}
	raw, err := c.doRaw("GET", "/api/memory/context?"+q.Encode(), nil)
	if err != nil {
		return fail("%v", err)
	}
	if f.on["--json"] {
		fmt.Println(strings.TrimSpace(string(raw)))
		return 0
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return fail("unexpected reply from the server: %v", err)
	}
	if out.Context == "" {
		return 0
	}
	if name := f.str("--agent", ""); name != "" {
		home, _ := os.UserHomeDir()
		p, err := agentprofile.Load(name, home)
		if err != nil {
			return fail("%v", err)
		}
		if p.Output == "claude-json" {
			native := map[string]string{"prompt": "prompt", "action": "pre_action"}[event]
			hookEvent := p.Hooks.Events[native]
			if hookEvent == "" {
				hookEvent = map[string]string{"prompt": "UserPromptSubmit", "action": "PreToolUse"}[event]
			}
			b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{
				"hookEventName": hookEvent, "additionalContext": out.Context}})
			fmt.Println(string(b))
			return 0
		}
	}
	fmt.Println(out.Context)
	return 0
}
