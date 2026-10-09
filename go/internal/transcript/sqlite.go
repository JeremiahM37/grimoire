package transcript

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite"
)

func openRO(path string) (*sql.DB, error) {
	// mode=ro: never lock or write another program's live database.
	return sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&_pragma=busy_timeout(2000)")
}

// readOpenCode reads opencode's SQLite store (session, message, part tables;
// message.data and part.data are JSON).
func readOpenCode(path string) ([]Session, error) {
	db, err := openRO(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, COALESCE(directory,''), COALESCE(model,''), COALESCE(time_created,0), COALESCE(time_updated,0) FROM session ORDER BY time_created`)
	if err != nil {
		return nil, fmt.Errorf("not an opencode database: %w", err)
	}
	type srow struct {
		id, dir, model string
		created, upd   float64
	}
	var sess []srow
	for rows.Next() {
		var r srow
		if err := rows.Scan(&r.id, &r.dir, &r.model, &r.created, &r.upd); err != nil {
			rows.Close()
			return nil, err
		}
		sess = append(sess, r)
	}
	rows.Close()
	var out []Session
	for _, r := range sess {
		b := newBuilder()
		b.s.ID, b.s.Cwd, b.s.Model = r.id, r.dir, modelName(r.model)
		b.s.Start, b.s.End = epoch(r.created), epoch(r.upd)
		parts := map[string][]obj{}
		pr, err := db.Query(`SELECT message_id, data FROM part WHERE session_id = ? ORDER BY time_created, id`, r.id)
		if err != nil {
			return nil, err
		}
		for pr.Next() {
			var mid, data string
			if pr.Scan(&mid, &data) == nil {
				var o obj
				if json.Unmarshal([]byte(data), &o) == nil {
					parts[mid] = append(parts[mid], o)
				}
			}
		}
		pr.Close()
		mr, err := db.Query(`SELECT id, COALESCE(time_created,0), data FROM message WHERE session_id = ? ORDER BY time_created, id`, r.id)
		if err != nil {
			return nil, err
		}
		for mr.Next() {
			var mid, data string
			var created float64
			if mr.Scan(&mid, &created, &data) != nil {
				continue
			}
			var msg obj
			if json.Unmarshal([]byte(data), &msg) != nil {
				continue
			}
			at := epoch(created)
			var text []string
			for _, p := range parts[mid] {
				switch str(p, "type") {
				case "text":
					if p["synthetic"] != true && p["ignored"] != true {
						text = append(text, str(p, "text"))
					}
				case "tool":
					st := asObj(p["state"])
					b.tool("", str(p, "tool"), targetOf(st["input"]), at)
					if str(st, "status") == "error" {
						b.s.Turns[len(b.s.Turns)-1].ToolError = true
					}
				}
			}
			switch str(msg, "role") {
			case "user":
				b.say(RoleUser, strings.Join(text, "\n"), at)
			case "assistant":
				if m := str(msg, "modelID"); m != "" {
					b.s.Model = m
				}
				b.say(RoleAssistant, strings.Join(text, "\n"), at)
			}
		}
		mr.Close()
		out = append(out, b.s)
	}
	return out, nil
}

// modelName reads opencode's session.model, a name or a JSON object.
func modelName(s string) string {
	if strings.HasPrefix(s, "{") {
		var o obj
		if json.Unmarshal([]byte(s), &o) == nil {
			for _, k := range []string{"modelID", "id", "name"} {
				if v := str(o, k); v != "" {
					return v
				}
			}
		}
	}
	return s
}

// readCursor reads Cursor's state.vscdb: a cursorDiskKV table whose
// composerData:<id> rows describe a conversation and whose
// bubbleId:<id>:<bubble> rows hold the messages (type 1 user, 2 assistant; a
// tool call is toolFormerData on an assistant bubble). Older builds inline the
// bubbles in composerData.conversation.
func readCursor(path string) ([]Session, error) {
	db, err := openRO(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT key, value FROM cursorDiskKV WHERE key LIKE 'composerData:%'`)
	if err != nil {
		return nil, fmt.Errorf("not a cursor database: %w", err)
	}
	type comp struct {
		id string
		o  obj
	}
	var comps []comp
	for rows.Next() {
		var k string
		var v any
		if rows.Scan(&k, &v) != nil {
			continue
		}
		var o obj
		if json.Unmarshal([]byte(asString(v)), &o) == nil {
			comps = append(comps, comp{strings.TrimPrefix(k, "composerData:"), o})
		}
	}
	rows.Close()
	var out []Session
	for _, c := range comps {
		b := newBuilder()
		b.s.ID = c.id
		if cfg := asObj(c.o["modelConfig"]); cfg != nil {
			if m := str(cfg, "modelName"); m != "" && m != "default" {
				b.s.Model = m
			}
		}
		b.s.Start = toTime(c.o["createdAt"])
		b.s.End = toTime(c.o["lastUpdatedAt"])
		var bubbles []obj
		for _, h := range asList(c.o["fullConversationHeadersOnly"]) {
			id := str(asObj(h), "bubbleId")
			if id == "" {
				continue
			}
			var v any
			if db.QueryRow(`SELECT value FROM cursorDiskKV WHERE key = ?`, "bubbleId:"+c.id+":"+id).Scan(&v) != nil {
				continue
			}
			var o obj
			if json.Unmarshal([]byte(asString(v)), &o) == nil {
				bubbles = append(bubbles, o)
			}
		}
		if len(bubbles) == 0 {
			for _, x := range asList(c.o["conversation"]) {
				if o := asObj(x); o != nil {
					bubbles = append(bubbles, o)
				}
			}
		}
		for _, bu := range bubbles {
			at := toTime(bu["createdAt"])
			if m := str(asObj(bu["modelInfo"]), "modelName"); m != "" && m != "default" && b.s.Model == "" {
				b.s.Model = m
			}
			switch typ, _ := bu["type"].(float64); typ {
			case 1:
				b.say(RoleUser, str(bu, "text"), at)
			case 2:
				if tf := asObj(bu["toolFormerData"]); tf != nil {
					b.tool("", cursorToolName(tf), cursorTarget(tf), at)
					if cursorFailed(tf) {
						b.s.Turns[len(b.s.Turns)-1].ToolError = true
					}
				}
				b.say(RoleAssistant, str(bu, "text"), at)
			}
		}
		out = append(out, b.s)
	}
	return out, nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []byte:
		return string(t)
	}
	return ""
}

func cursorToolName(tf obj) string {
	for _, k := range []string{"name", "toolName"} {
		if v := str(tf, k); v != "" {
			return v
		}
	}
	if n, ok := tf["tool"].(float64); ok {
		return fmt.Sprintf("tool_%d", int(n))
	}
	return "?"
}

func cursorTarget(tf obj) string {
	for _, k := range []string{"params", "rawArgs", "args", "input"} {
		switch v := tf[k].(type) {
		case string:
			var parsed any
			if json.Unmarshal([]byte(v), &parsed) == nil {
				if t := targetOf(parsed); t != "" {
					return t
				}
			}
		case map[string]any:
			if t := targetOf(v); t != "" {
				return t
			}
		}
	}
	return ""
}

func cursorFailed(tf obj) bool {
	if d := strings.ToLower(str(tf, "userDecision")); d == "rejected" {
		return true
	}
	status := strings.ToLower(str(tf, "status"))
	if status == "" {
		status = strings.ToLower(str(asObj(tf["additionalData"]), "status"))
	}
	switch status {
	case "error", "errored", "failed", "failure", "cancelled", "canceled", "aborted", "rejected", "denied", "timeout":
		return true
	}
	return false
}
