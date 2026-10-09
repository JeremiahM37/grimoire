package transcript

import (
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var (
	exitCodeRE = regexp.MustCompile(`(?:"exit_code"\s*:\s*|Process exited with code |Exit code: |exit code )(-?\d+)`)
	// Newer Codex runs shell commands through a JavaScript "exec" tool whose
	// input holds tools.exec_command({cmd:"..."}) calls.
	execCmdRE = regexp.MustCompile(`cmd\s*:\s*("(?:[^"\\]|\\.)*")`)
)

var codexSynthetic = []string{"<environment_context>", "<INSTRUCTIONS>", "<permissions", "<model_switch>", "<user_instructions>", "<skills_instructions>", "<collaboration_mode>", "# AGENTS.md instructions"}

// readCodex reads ~/.codex/sessions/YYYY/MM/DD/rollout-*.jsonl: lines of
// {timestamp, type, payload}; model I/O is in `response_item` payloads
// (message, function_call, custom_tool_call and their outputs), the model name
// in `turn_context`, the cwd in `session_meta`.
func readCodex(path string) ([]Session, error) {
	b := newBuilder()
	b.s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	err := eachLine(path, func(e obj) {
		at := toTime(e["timestamp"])
		p := asObj(e["payload"])
		switch str(e, "type") {
		case "session_meta":
			if id := str(p, "id"); id != "" {
				b.s.ID = id
			}
			if cwd := str(p, "cwd"); cwd != "" {
				b.s.Cwd = cwd
			}
		case "turn_context":
			if m := str(p, "model"); m != "" {
				b.s.Model = m
			}
			if cwd := str(p, "cwd"); cwd != "" && b.s.Cwd == "" {
				b.s.Cwd = cwd
			}
		case "response_item":
			codexItem(b, p, at)
		}
	})
	return b.session(), err
}

func codexItem(b *builder, p obj, at time.Time) {
	switch str(p, "type") {
	case "message":
		role := str(p, "role")
		if role != "user" && role != "assistant" {
			return
		}
		text := textOf(p["content"])
		if role == "user" {
			t := strings.TrimSpace(text)
			for _, pre := range codexSynthetic {
				if strings.HasPrefix(t, pre) {
					return
				}
			}
		}
		b.say(role, text, at)
	case "function_call":
		args := p["arguments"]
		if s, ok := args.(string); ok {
			var parsed any
			if json.Unmarshal([]byte(s), &parsed) == nil {
				args = parsed
			}
		}
		b.tool(str(p, "call_id"), str(p, "name"), codexTarget(args), at)
	case "custom_tool_call":
		in := str(p, "input")
		id, name := str(p, "call_id"), str(p, "name")
		cmds := execCmdRE.FindAllStringSubmatch(in, -1)
		if len(cmds) == 0 {
			b.tool(id, name, firstLine(in), at)
			return
		}
		for i, c := range cmds {
			var cmd string
			if json.Unmarshal([]byte(c[1]), &cmd) != nil {
				continue
			}
			key := id // a script's commands share one output: id, id#1, id#2 ...
			if id != "" && i > 0 {
				key = id + "#" + string(rune('0'+i))
			}
			b.tool(key, "exec_command", cmd, at)
		}
	case "function_call_output", "custom_tool_call_output":
		out := flatten(p["output"])
		failed := false
		for _, m := range exitCodeRE.FindAllStringSubmatch(out, -1) {
			if m[1] != "0" {
				failed = true
			}
		}
		id := str(p, "call_id")
		b.result(id, failed)
		for i := 1; i < 10; i++ {
			b.result(id+"#"+string(rune('0'+i)), failed)
		}
	}
}

func codexTarget(args any) string {
	if t := targetOf(args); t != "" {
		return t
	}
	if s, ok := args.(string); ok {
		return firstLine(s)
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// flatten renders a tool output (a string, or blocks) as one string.
func flatten(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if t := textOf(v); t != "" {
		return t
	}
	if v == nil {
		return ""
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
