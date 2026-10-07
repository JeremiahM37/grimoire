package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func rpc(t *testing.T, h http.Handler, path string, header map[string]string, method string, params any) map[string]any {
	t.Helper()
	frame := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		frame["params"] = params
	}
	b, _ := json.Marshal(frame)
	req := httptest.NewRequest("POST", path, bytes.NewReader(b))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%s %s = %d %s", path, method, rec.Code, rec.Body)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func toolNames(resp map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, x := range resp["result"].(map[string]any)["tools"].([]any) {
		m := x.(map[string]any)
		out[m["name"].(string)] = m
	}
	return out
}

func TestPerBankMCPEndpoints(t *testing.T) {
	var seen []seenReq
	mcpTools := ""
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, seenReq{r.Method, r.URL.RequestURI(), body})
		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/banks/") && strings.Count(r.URL.Path, "/") == 3 {
			_, _ = w.Write([]byte(`{"bank_id":"x","config":{"mcp_tools":"` + mcpTools + `"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	s.Session, s.Bank = "", ""
	h := s.HTTPHandler()

	// Single-bank: no bank argument, no bank-management tools.
	tools := toolNames(rpc(t, h, "/mcp/team:alpha", nil, "tools/list", nil))
	if _, ok := tools["list_banks"]; ok {
		t.Error("list_banks offered on a single-bank endpoint")
	}
	props := tools["reflect"]["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["bank"]; ok {
		t.Error("the bank argument must be hidden on a single-bank endpoint")
	}
	if _, ok := tools["search_notes"]; !ok {
		t.Error("non-bank tools stay available")
	}
	// A call there goes to the path's bank, whatever the agent passes.
	seen = nil
	rpc(t, h, "/mcp/team:alpha", nil, "tools/call", map[string]any{"name": "reflect",
		"arguments": map[string]any{"query": "who owns billing?", "bank": "other"}})
	if last := seen[len(seen)-1]; last.path != "/api/banks/team:alpha/reflect" || last.body["query"] != "who owns billing?" {
		t.Errorf("reflect went to %s %v", last.path, last.body)
	}
	resp := rpc(t, h, "/mcp/team:alpha", nil, "tools/call", map[string]any{"name": "list_banks", "arguments": map[string]any{}})
	if res := resp["result"].(map[string]any); res["isError"] != true {
		t.Errorf("list_banks on a single-bank endpoint = %v", res)
	}

	// /mcp with X-Bank-Id: a default the argument may override.
	seen = nil
	rpc(t, h, "/mcp", map[string]string{"X-Bank-Id": "dflt"}, "tools/call", map[string]any{"name": "list_observations",
		"arguments": map[string]any{}})
	rpc(t, h, "/mcp", map[string]string{"X-Bank-Id": "dflt"}, "tools/call", map[string]any{"name": "list_directives",
		"arguments": map[string]any{"bank": "explicit"}})
	var paths []string
	for _, r := range seen {
		paths = append(paths, r.path)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, "/api/banks/dflt/observations") || !strings.Contains(joined, "/api/banks/explicit/directives") {
		t.Errorf("paths = %v", paths)
	}

	// The bank's allowlist narrows what the endpoint offers and accepts.
	mcpTools = "bank_recall,reflect"
	s.allow = &allowCache{at: map[string]time.Time{}, list: map[string]map[string]bool{}} // drop cached lists
	tools = toolNames(rpc(t, h, "/mcp/locked", nil, "tools/list", nil))
	if len(tools) != 2 || tools["reflect"] == nil || tools["bank_recall"] == nil {
		t.Errorf("allowlisted tools = %v", keys(tools))
	}
	resp = rpc(t, h, "/mcp/locked", nil, "tools/call", map[string]any{"name": "retain", "arguments": map[string]any{"content": "x"}})
	if res := resp["result"].(map[string]any); res["isError"] != true {
		t.Errorf("a tool outside the allowlist ran: %v", res)
	}

	if rec := httptest.NewRecorder(); true {
		h.ServeHTTP(rec, httptest.NewRequest("POST", "/mcp/Bad%20Bank", strings.NewReader(`{}`)))
		if rec.Code != 400 {
			t.Errorf("invalid bank in path = %d", rec.Code)
		}
	}
}

func keys(m map[string]map[string]any) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestMCPRetainIsAsyncAndToolsAreWired(t *testing.T) {
	s, seen := bankStub(t)
	s.Bank = "b"
	if _, err := s.dispatch("retain", map[string]any{"content": "a transcript"}); err != nil {
		t.Fatal(err)
	}
	if (*seen)[0].body["async"] != true {
		t.Errorf("retain body = %v", (*seen)[0].body)
	}
	cases := map[string]string{
		"consolidate":          "POST /api/banks/b/consolidate",
		"get_mental_model":     "GET /api/banks/b/mental-models/people%2Fdana",
		"refresh_mental_model": "POST /api/banks/b/mental-models/people%2Fdana/refresh",
		"get_operation":        "GET /api/banks/b/operations/op-1",
		"cancel_operation":     "DELETE /api/banks/b/operations/op-1",
		"import_bank_template": "POST /api/banks/b/import?dry_run=1",
		"list_bank_templates":  "GET /api/bank-templates",
	}
	for name, want := range cases {
		*seen = nil
		args := map[string]any{"id": "people/dana", "operation_id": "op-1", "template": "research", "dry_run": true}
		if _, err := s.dispatch(name, args); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := (*seen)[0].method + " " + (*seen)[0].path
		if got != want {
			t.Errorf("%s → %s, want %s", name, got, want)
		}
	}
}
