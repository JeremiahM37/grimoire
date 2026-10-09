package transcript

import (
	"path/filepath"
	"strings"
)

// readPi reads a pi.dev session (format v3): entries form a tree through
// id/parentId inside one file. The active branch is the path from the root to
// the newest leaf; a compaction entry hides what came before its
// firstKeptEntryId.
func readPi(path string) ([]Session, error) {
	b := newBuilder()
	b.s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	byID := map[string]obj{}
	hasChild := map[string]bool{}
	var header obj
	var order []string
	err := eachLine(path, func(e obj) {
		id := str(e, "id")
		if id == "" {
			return
		}
		byID[id] = e
		order = append(order, id)
		if str(e, "type") == "session" {
			header = e
		}
		if pid := str(e, "parentId"); pid != "" {
			hasChild[pid] = true
		}
	})
	if err != nil || len(byID) == 0 {
		return b.session(), err
	}
	if header != nil {
		if id := str(header, "id"); id != "" {
			b.s.ID = id
		}
		b.s.Cwd = str(header, "cwd")
	}
	var leaf string
	for _, id := range order {
		if hasChild[id] || str(byID[id], "type") == "session" && len(byID) > 1 {
			continue
		}
		if leaf == "" || !toTime(byID[id]["timestamp"]).Before(toTime(byID[leaf]["timestamp"])) {
			leaf = id
		}
	}
	var chain []obj
	seen := map[string]bool{}
	for cur := leaf; cur != "" && !seen[cur]; {
		seen[cur] = true
		e, ok := byID[cur]
		if !ok {
			break
		}
		chain = append([]obj{e}, chain...)
		cur = str(e, "parentId")
	}
	cut := ""
	for _, e := range chain {
		if str(e, "type") == "compaction" {
			if k := str(e, "firstKeptEntryId"); k != "" {
				cut = k
			}
		}
	}
	keep := cut == ""
	for _, e := range chain {
		if !keep && str(e, "id") == cut {
			keep = true
		}
		if !keep || str(e, "type") != "message" {
			continue
		}
		at := toTime(e["timestamp"])
		msg := asObj(e["message"])
		switch role := str(msg, "role"); role {
		case "user":
			b.say(RoleUser, textOf(msg["content"]), at)
		case "assistant":
			if m := str(msg, "model"); m != "" {
				b.s.Model = m
			}
			var text []string
			var calls []obj
			for _, blk := range asList(msg["content"]) {
				bo := asObj(blk)
				switch str(bo, "type") {
				case "text":
					text = append(text, str(bo, "text"))
				case "toolCall":
					calls = append(calls, bo)
				}
			}
			b.say(RoleAssistant, strings.Join(text, "\n"), at)
			for _, bo := range calls {
				b.tool(str(bo, "id"), str(bo, "name"), targetOf(bo["arguments"]), at)
			}
		case "toolResult":
			failed := msg["isError"] == true || msg["is_error"] == true
			b.result(str(msg, "toolCallId"), failed)
		case "bashExecution":
			cmd := textOf(msg["content"])
			if c := str(msg, "command"); c != "" {
				cmd = c
			}
			b.tool("", "bash", cmd, at)
			if code, ok := msg["exitCode"].(float64); ok && code != 0 {
				b.s.Turns[len(b.s.Turns)-1].ToolError = true
			}
		}
	}
	return b.session(), nil
}
