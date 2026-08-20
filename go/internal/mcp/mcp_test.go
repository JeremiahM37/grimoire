package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stubAPI records what the MCP layer asks the HTTP API for.
func stubAPI(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) *Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(ts.Close)
	return New(ts.URL, "test-agent")
}

func call(t *testing.T, s *Server, frames ...map[string]any) []map[string]any {
	t.Helper()
	var in bytes.Buffer
	for _, f := range frames {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		in.Write(b)
		in.WriteByte('\n')
	}
	var out bytes.Buffer
	if err := s.Serve(&in, &out); err != nil {
		t.Fatal(err)
	}
	var resps []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad response frame %q: %v", line, err)
		}
		resps = append(resps, m)
	}
	return resps
}

// The instructions are how an agent learns the knowledge base is worth
// consulting; a server that omits them gets ignored in practice.
func TestInitializeAdvertisesInstructions(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if len(resps) != 1 {
		t.Fatalf("got %d responses", len(resps))
	}
	res := resps[0]["result"].(map[string]any)
	inst, _ := res["instructions"].(string)
	for _, want := range []string{"get_briefing", "before assuming", "remember"} {
		if !strings.Contains(strings.ToLower(inst), strings.ToLower(want)) {
			t.Errorf("instructions missing %q: %q", want, inst)
		}
	}
	if res["protocolVersion"] != ProtocolVersion {
		t.Errorf("protocolVersion = %v", res["protocolVersion"])
	}
}

func TestToolsListIsComplete(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := resps[0]["result"].(map[string]any)["tools"].([]any)

	got := map[string]bool{}
	for _, tl := range tools {
		m := tl.(map[string]any)
		name := m["name"].(string)
		got[name] = true
		if len(m["description"].(string)) < 40 {
			t.Errorf("%s: description too thin to guide an agent", name)
		}
		if _, ok := m["inputSchema"].(map[string]any); !ok {
			t.Errorf("%s: missing inputSchema", name)
		}
	}
	for _, want := range []string{
		"get_briefing", "kb_info", "search_notes", "ask_notes", "read_note",
		"list_notes", "create_note", "update_note", "append_daily", "backlinks",
		"list_tags", "get_fact", "set_fact", "remember", "recall", "consolidate_memory",
		"list_grants", "use_credential",
	} {
		if !got[want] {
			t.Errorf("tool %q not advertised", want)
		}
	}
}

func TestToolCallsHitTheExpectedEndpoints(t *testing.T) {
	var seen []string
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	for _, tc := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"kb_info", nil, "GET /api/health"},
		{"get_briefing", nil, "GET /api/briefing"},
		{"search_notes", map[string]any{"query": "x"}, "GET /api/search"},
		{"list_tags", nil, "GET /api/tags"},
		{"remember", map[string]any{"text": "x"}, "POST /api/memory"},
		{"recall", map[string]any{"query": "x"}, "GET /api/memory"},
		{"get_fact", map[string]any{"key": "port"}, "GET /api/facts"},
	} {
		seen = nil
		call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": tc.tool, "arguments": tc.args}})
		if len(seen) != 1 || seen[0] != tc.want {
			t.Errorf("%s hit %v, want %s", tc.tool, seen, tc.want)
		}
	}
}

// An agent must not be able to write memories under someone else's name.
func TestRememberAttributionIsServerControlled(t *testing.T) {
	var body map[string]any
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true}`))
	})
	call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "remember", "arguments": map[string]any{
			"text": "x", "agent": "someone-else"}}})
	if body["agent"] != "test-agent" {
		t.Errorf("agent = %v, want the server's configured identity", body["agent"])
	}
}

// A failing tool should come back as a result the agent can read and adapt to,
// not as a protocol error that looks like a transport fault.
func TestToolFailureIsReportedAsResult(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"detail":"vault locked"}`, http.StatusLocked)
	})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_grants"}})
	res := resps[0]["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("expected isError, got %v", res)
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "vault locked") {
		t.Errorf("error text should carry the reason: %q", text)
	}
	if resps[0]["error"] != nil {
		t.Errorf("tool failure must not be a protocol error: %v", resps[0]["error"])
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "nope"}})
	if resps[0]["result"].(map[string]any)["isError"] != true {
		t.Error("unknown tool should report isError")
	}
	resps = call(t, s, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "bogus"})
	if resps[0]["error"] == nil {
		t.Error("unknown method should be a protocol error")
	}
}

// Notifications carry no id and must not be answered.
func TestNotificationsAreNotAnswered(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if len(resps) != 0 {
		t.Errorf("notification produced %d responses", len(resps))
	}
}

// An instance whose administrative surface is gated must still be usable by an
// agent: the tools that touch it have to carry the token, or they fail with a
// 401 an agent cannot act on.
func TestAdminTokenIsForwarded(t *testing.T) {
	var gotAdmin, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAdmin = r.Header.Get("X-Grimoire-Admin")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	s := New(srv.URL, "test-agent")
	s.AuthToken = "read-token"
	s.AdminToken = "admin-token"
	if _, err := s.dispatch("list_grants", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	if gotAdmin != "admin-token" {
		t.Errorf("admin token not forwarded: %q", gotAdmin)
	}
	if gotAuth != "Bearer read-token" {
		t.Errorf("auth token not forwarded: %q", gotAuth)
	}
}
