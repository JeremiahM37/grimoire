package transcript

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
)

// Every fixture here is synthetic. No real transcript belongs in the repo.

var secret = "ghp_" + strings.Repeat("aB3dE5gH7j", 4)

func writeLines(t *testing.T, path string, lines ...any) {
	t.Helper()
	var b strings.Builder
	for _, l := range lines {
		raw, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	b.WriteString("{not json\n") // a torn line is normal and must not stop the read
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

type m = map[string]any
type l = []any

func read(t *testing.T, spec Spec, path string) []Session {
	t.Helper()
	ss, err := ReadFile(spec, path)
	if err != nil {
		t.Fatal(err)
	}
	return ss
}

func noSecrets(t *testing.T, ss []Session) {
	t.Helper()
	for _, s := range ss {
		for _, tn := range s.Turns {
			if strings.Contains(tn.Text+tn.ToolTarget, secret) {
				t.Errorf("secret survived in %+v", tn)
			}
		}
	}
}

func TestFormatsMatchTheProfileValidator(t *testing.T) {
	if !reflect.DeepEqual(Formats, agentprofile.TranscriptFormats) {
		t.Fatalf("transcript.Formats %v != agentprofile.TranscriptFormats %v", Formats, agentprofile.TranscriptFormats)
	}
}

func TestClaudeJSONL(t *testing.T) {
	p := filepath.Join(t.TempDir(), "abc.jsonl")
	writeLines(t, p,
		m{"type": "user", "sessionId": "s1", "cwd": "/w/proj", "timestamp": "2026-10-01T10:00:00Z",
			"message": m{"role": "user", "content": "deploy it, token " + secret}},
		m{"type": "user", "isMeta": true, "timestamp": "2026-10-01T10:00:01Z", "message": m{"role": "user", "content": "injected"}},
		m{"type": "assistant", "isSidechain": true, "message": m{"role": "assistant", "content": "sub agent chatter"}},
		m{"type": "assistant", "timestamp": "2026-10-01T10:00:05Z", "message": m{"role": "assistant", "model": "claude-x",
			"content": l{m{"type": "text", "text": "running"}, m{"type": "tool_use", "id": "t1", "name": "Bash", "input": m{"command": "make deploy"}}}}},
		m{"type": "user", "timestamp": "2026-10-01T10:00:09Z", "message": m{"role": "user",
			"content": l{m{"type": "tool_result", "tool_use_id": "t1", "is_error": true, "content": "boom"}}}},
		m{"type": "user", "timestamp": "2026-10-01T10:05:00Z", "message": m{"role": "user", "content": l{m{"type": "text", "text": "no, use staging"}}}},
		m{"type": "user", "timestamp": "2026-10-01T10:05:01Z", "message": m{"role": "user", "content": "<command-name>/clear</command-name>"}},
	)
	ss := read(t, Spec{Agent: "claude-code", Format: "claude-jsonl"}, p)
	if len(ss) != 1 {
		t.Fatalf("%d sessions", len(ss))
	}
	s := ss[0]
	if s.ID != "s1" || s.Cwd != "/w/proj" || s.Model != "claude-x" || s.Agent != "claude-code" {
		t.Errorf("%+v", s)
	}
	if s.Prompts() != 2 || s.ToolCalls() != 1 || s.ToolErrors() != 1 {
		t.Errorf("prompts=%d tools=%d errors=%d turns=%+v", s.Prompts(), s.ToolCalls(), s.ToolErrors(), s.Turns)
	}
	if s.Turns[2].Tool != "Bash" || s.Turns[2].ToolTarget != "make deploy" {
		t.Errorf("tool turn %+v", s.Turns[2])
	}
	if s.Duration().Minutes() != 5 {
		t.Errorf("duration %v", s.Duration())
	}
	noSecrets(t, ss)
}

func TestCodexRollout(t *testing.T) {
	p := filepath.Join(t.TempDir(), "rollout-2026-10-01T10-00-00-uuid.jsonl")
	item := func(ts string, payload m) m { return m{"timestamp": ts, "type": "response_item", "payload": payload} }
	writeLines(t, p,
		m{"timestamp": "2026-10-01T10:00:00Z", "type": "session_meta", "payload": m{"id": "cx1", "cwd": "/w/cx"}},
		m{"timestamp": "2026-10-01T10:00:00Z", "type": "turn_context", "payload": m{"model": "gpt-x"}},
		item("2026-10-01T10:00:01Z", m{"type": "message", "role": "developer", "content": l{m{"type": "input_text", "text": "dev"}}}),
		item("2026-10-01T10:00:02Z", m{"type": "message", "role": "user", "content": l{m{"type": "input_text", "text": "<environment_context>x</environment_context>"}}}),
		item("2026-10-01T10:00:03Z", m{"type": "message", "role": "user", "content": l{m{"type": "input_text", "text": "run tests, key " + secret}}}),
		item("2026-10-01T10:00:04Z", m{"type": "function_call", "name": "shell", "call_id": "c1", "arguments": `{"command":["bash","-lc","go test ./..."]}`}),
		item("2026-10-01T10:00:05Z", m{"type": "function_call_output", "call_id": "c1", "output": `{"output":"FAIL","metadata":{"exit_code":1}}`}),
		item("2026-10-01T10:00:06Z", m{"type": "custom_tool_call", "name": "exec", "call_id": "c2",
			"input": `text(await tools.exec_command({cmd:"git status"}));text(await tools.exec_command({cmd:"ls \"a b\""}));`}),
		item("2026-10-01T10:00:07Z", m{"type": "custom_tool_call_output", "call_id": "c2",
			"output": l{m{"type": "input_text", "text": `{"exit_code":0,"output":"ok"}`}}}),
		item("2026-10-01T10:00:08Z", m{"type": "message", "role": "assistant", "content": l{m{"type": "output_text", "text": "done"}}}),
	)
	ss := read(t, Spec{Agent: "codex", Format: "codex-rollout"}, p)
	s := ss[0]
	if s.ID != "cx1" || s.Cwd != "/w/cx" || s.Model != "gpt-x" {
		t.Errorf("%+v", s)
	}
	if s.Prompts() != 1 || s.ToolCalls() != 3 || s.ToolErrors() != 1 {
		t.Fatalf("prompts=%d tools=%d errors=%d %+v", s.Prompts(), s.ToolCalls(), s.ToolErrors(), s.Turns)
	}
	var targets []string
	for _, tn := range s.Turns {
		if tn.Role == RoleTool {
			targets = append(targets, tn.ToolTarget)
		}
	}
	if !reflect.DeepEqual(targets, []string{"go test ./...", "git status", `ls "a b"`}) {
		t.Errorf("targets %q", targets)
	}
	noSecrets(t, ss)
}

func TestPiBranchAndCompaction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "s.jsonl")
	msg := func(id, parent, ts string, message m) m {
		return m{"type": "message", "id": id, "parentId": parent, "timestamp": ts, "message": message}
	}
	writeLines(t, p,
		m{"type": "session", "version": 3, "id": "pi1", "timestamp": "2026-10-01T10:00:00Z", "cwd": "/w/pi"},
		msg("a", "pi1", "2026-10-01T10:00:01Z", m{"role": "user", "content": "old question"}),
		msg("b", "a", "2026-10-01T10:00:02Z", m{"role": "user", "content": "abandoned branch"}),
		msg("c", "a", "2026-10-01T10:00:03Z", m{"role": "assistant", "model": "pi-m", "content": l{
			m{"type": "text", "text": "ok"}, m{"type": "toolCall", "id": "k1", "name": "bash", "arguments": m{"command": "ls -la"}}}}),
		msg("d", "c", "2026-10-01T10:00:04Z", m{"role": "toolResult", "toolCallId": "k1", "isError": true, "content": "denied"}),
		msg("e", "d", "2026-10-01T10:00:05Z", m{"role": "bashExecution", "command": "echo " + secret, "exitCode": 2}),
		msg("f", "e", "2026-10-01T10:00:06Z", m{"role": "user", "content": l{m{"type": "text", "text": "thanks"}}}),
	)
	ss := read(t, Spec{Agent: "pi", Format: "pi"}, p)
	s := ss[0]
	if s.ID != "pi1" || s.Cwd != "/w/pi" || s.Model != "pi-m" {
		t.Errorf("%+v", s)
	}
	for _, tn := range s.Turns {
		if strings.Contains(tn.Text, "abandoned") {
			t.Error("the abandoned branch must not be read")
		}
	}
	if s.ToolCalls() != 2 || s.ToolErrors() != 2 || s.Prompts() != 2 {
		t.Errorf("tools=%d errors=%d prompts=%d", s.ToolCalls(), s.ToolErrors(), s.Prompts())
	}
	noSecrets(t, ss)
}

func TestGenericJSONLFieldMap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "new-agent.jsonl")
	writeLines(t, p,
		m{"conv": "g1", "ts": 1790000000, "who": "human", "body": "hi " + secret, "wd": "/w/g"},
		m{"conv": "g1", "ts": 1790000010, "who": "ai", "body": l{m{"type": "text", "text": "hello"}}},
		m{"conv": "g1", "ts": 1790000020, "call": "shell", "args": m{"command": "ls"}, "failed": 1},
		m{"conv": "g2", "ts": 1790000030, "who": "human", "body": "other session"},
	)
	spec := Spec{Agent: "newagent", Format: "generic-jsonl", Map: map[string]string{
		"session": "conv", "time": "ts", "role": "who", "text": "body", "cwd": "wd",
		"tool": "call", "tool_target": "args", "tool_error": "failed"}}
	ss := read(t, spec, p)
	if len(ss) != 2 || ss[0].ID != "g1" || ss[1].ID != "g2" {
		t.Fatalf("%+v", ss)
	}
	s := ss[0]
	if s.Cwd != "/w/g" || s.Prompts() != 1 || s.ToolErrors() != 1 || s.Turns[2].ToolTarget != "ls" || s.Duration().Seconds() != 20 {
		t.Errorf("%+v", s)
	}
	noSecrets(t, ss)
	if _, err := ReadFile(Spec{Format: "generic-jsonl"}, p); err == nil {
		t.Error("a generic reader with no map must say so")
	}
}

func openRW(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func exec(t *testing.T, db *sql.DB, q string, a ...any) {
	t.Helper()
	if _, err := db.Exec(q, a...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func jsonStr(v any) string { raw, _ := json.Marshal(v); return string(raw) }

func TestOpenCodeSQLite(t *testing.T) {
	p := filepath.Join(t.TempDir(), "opencode.db")
	db := openRW(t, p)
	exec(t, db, `CREATE TABLE session (id TEXT, directory TEXT, parent_id TEXT, agent TEXT, model TEXT, time_created INTEGER, time_updated INTEGER)`)
	exec(t, db, `CREATE TABLE message (id TEXT, session_id TEXT, time_created INTEGER, data TEXT)`)
	exec(t, db, `CREATE TABLE part (id TEXT, message_id TEXT, session_id TEXT, time_created INTEGER, data TEXT)`)
	exec(t, db, `INSERT INTO session VALUES ('o1','/w/oc',NULL,'build','gpt-oc',1790000000000,1790000090000)`)
	exec(t, db, `INSERT INTO message VALUES ('m1','o1',1790000001000,?)`, jsonStr(m{"role": "user"}))
	exec(t, db, `INSERT INTO message VALUES ('m2','o1',1790000002000,?)`, jsonStr(m{"role": "assistant", "modelID": "oc-model"}))
	exec(t, db, `INSERT INTO part VALUES ('p1','m1','o1',1790000001000,?)`, jsonStr(m{"type": "text", "text": "fix the build " + secret}))
	exec(t, db, `INSERT INTO part VALUES ('p2','m2','o1',1790000002000,?)`, jsonStr(m{"type": "text", "text": "on it"}))
	exec(t, db, `INSERT INTO part VALUES ('p3','m2','o1',1790000003000,?)`, jsonStr(m{"type": "tool", "tool": "bash",
		"state": m{"status": "error", "input": m{"command": "make"}}}))
	db.Close()
	ss := read(t, Spec{Agent: "opencode", Format: "opencode"}, p)
	if len(ss) != 1 {
		t.Fatalf("%+v", ss)
	}
	s := ss[0]
	if s.ID != "o1" || s.Cwd != "/w/oc" || s.Model != "oc-model" || s.Prompts() != 1 || s.ToolCalls() != 1 || s.ToolErrors() != 1 {
		t.Errorf("%+v", s)
	}
	if s.Duration().Seconds() != 90 {
		t.Errorf("duration %v", s.Duration())
	}
	noSecrets(t, ss)
}

func TestCursorVSCDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.vscdb")
	db := openRW(t, p)
	exec(t, db, `CREATE TABLE cursorDiskKV (key TEXT PRIMARY KEY, value TEXT)`)
	put := func(k string, v any) { exec(t, db, `INSERT INTO cursorDiskKV VALUES (?,?)`, k, jsonStr(v)) }
	put("composerData:c1", m{"createdAt": "2026-10-01T10:00:00Z", "modelConfig": m{"modelName": "cur-model"},
		"fullConversationHeadersOnly": l{m{"bubbleId": "b1"}, m{"bubbleId": "b2"}, m{"bubbleId": "b3"}}})
	put("bubbleId:c1:b1", m{"type": 1, "text": "add a flag " + secret, "createdAt": "2026-10-01T10:00:01Z"})
	put("bubbleId:c1:b2", m{"type": 2, "text": "", "createdAt": "2026-10-01T10:00:02Z",
		"toolFormerData": m{"name": "run_terminal_command_v2", "status": "error", "params": `{"command":"npm test"}`}})
	put("bubbleId:c1:b3", m{"type": 2, "text": "fixed", "createdAt": "2026-10-01T10:02:00Z"})
	// Legacy shape: bubbles inlined.
	put("composerData:c2", m{"conversation": l{m{"type": 1, "text": "legacy hello"}}})
	db.Close()
	ss := read(t, Spec{Agent: "cursor", Format: "cursor"}, p)
	if len(ss) != 2 {
		t.Fatalf("%d sessions", len(ss))
	}
	var c1 Session
	for _, s := range ss {
		if s.ID == "c1" {
			c1 = s
		}
	}
	if c1.Model != "cur-model" || c1.Prompts() != 1 || c1.ToolCalls() != 1 || c1.ToolErrors() != 1 || c1.Turns[1].ToolTarget != "npm test" {
		t.Errorf("%+v", c1)
	}
	noSecrets(t, ss)
}

func TestFilesGlobAndReadAllSince(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"2026/10/01/rollout-a.jsonl", "2026/10/02/rollout-b.jsonl", "2026/10/02/other.txt"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, f)), 0o755); err != nil {
			t.Fatal(err)
		}
		writeLines(t, filepath.Join(dir, f), m{"timestamp": "2026-10-01T10:00:00Z", "type": "response_item",
			"payload": m{"type": "message", "role": "user", "content": l{m{"type": "input_text", "text": "hi"}}}})
	}
	files, err := Files(filepath.Join(dir, "**/rollout-*.jsonl"))
	if err != nil || len(files) != 2 {
		t.Fatalf("%v %v", files, err)
	}
	ss, errs := ReadAll(Spec{Agent: "codex", Format: "codex-rollout", Glob: filepath.Join(dir, "**/rollout-*.jsonl")}, Options{})
	if len(ss) != 2 || len(errs) != 0 {
		t.Fatalf("%d %v", len(ss), errs)
	}
	if got, _ := Files(filepath.Join(dir, "2026/10/*/rollout-*.jsonl")); len(got) != 2 {
		t.Errorf("plain glob: %v", got)
	}
}
