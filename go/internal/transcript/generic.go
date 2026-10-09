package transcript

import (
	"fmt"
	"path/filepath"
	"strings"
)

// readGeneric reads any agent that writes one JSON object per line, described
// by a field map (the profile's transcripts.map). Each line becomes one turn.
//
//	session      session id (default: the file name)
//	cwd          working directory
//	time         timestamp (ISO string, or epoch seconds/milliseconds)
//	model        model name
//	role         "user" / "assistant" (other values: see role_user / role_assistant)
//	text         message text (a string, or content blocks)
//	tool         tool name; a line with one is a tool call
//	tool_target  the command or path (a string, argv list or object)
//	tool_error   true when the tool failed (a bool, or a non-zero number)
//	role_user, role_assistant   comma-separated role values that mean user /
//	                            assistant (defaults: user,human / assistant,ai,model)
//
// A line with neither a tool nor text is skipped. Sessions are split by the
// `session` field; with no such field the file is one session.
func readGeneric(path string, m map[string]string) ([]Session, error) {
	if m["role"] == "" && m["text"] == "" && m["tool"] == "" {
		return nil, fmt.Errorf("generic-jsonl needs a transcripts.map with at least role and text, or tool")
	}
	userVals := valueSet(m["role_user"], "user,human")
	asstVals := valueSet(m["role_assistant"], "assistant,ai,model,agent")
	byID := map[string]*builder{}
	var order []string
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	err := eachLine(path, func(e obj) {
		id := base
		if p := m["session"]; p != "" {
			if v := fmt.Sprint(orEmpty(dig(e, p))); v != "" {
				id = v
			}
		}
		b := byID[id]
		if b == nil {
			b = newBuilder()
			b.s.ID = id
			byID[id] = b
			order = append(order, id)
		}
		at := toTime(dig(e, m["time"]))
		if p := m["cwd"]; p != "" && b.s.Cwd == "" {
			if v, ok := dig(e, p).(string); ok {
				b.s.Cwd = v
			}
		}
		if p := m["model"]; p != "" {
			if v, ok := dig(e, p).(string); ok && v != "" {
				b.s.Model = v
			}
		}
		if p := m["tool"]; p != "" {
			if name, ok := dig(e, p).(string); ok && name != "" {
				target := ""
				if tp := m["tool_target"]; tp != "" {
					target = flattenTarget(dig(e, tp))
				}
				b.tool("", name, target, at)
				if ep := m["tool_error"]; ep != "" {
					b.s.Turns[len(b.s.Turns)-1].ToolError = truthy(dig(e, ep))
				}
				return
			}
		}
		role := strings.ToLower(fmt.Sprint(orEmpty(dig(e, m["role"]))))
		text := textOf(dig(e, m["text"]))
		switch {
		case userVals[role]:
			b.say(RoleUser, text, at)
		case asstVals[role]:
			b.say(RoleAssistant, text, at)
		}
	})
	var out []Session
	for _, id := range order {
		out = append(out, byID[id].s)
	}
	return out, err
}

func orEmpty(v any) any {
	if v == nil {
		return ""
	}
	return v
}

func valueSet(csv, def string) map[string]bool {
	if csv == "" {
		csv = def
	}
	out := map[string]bool{}
	for _, v := range strings.Split(csv, ",") {
		out[strings.ToLower(strings.TrimSpace(v))] = true
	}
	return out
}

func flattenTarget(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []any:
		return joinCommand(t)
	case map[string]any:
		return targetOf(t)
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t == "true" || t == "error" || t == "failed"
	}
	return false
}
