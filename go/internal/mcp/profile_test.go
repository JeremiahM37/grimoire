package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseToolProfile(t *testing.T) {
	for _, raw := range []string{"", "  ", "all", "ALL", "core,all"} {
		p, err := ParseToolProfile(raw)
		if err != nil || p != nil {
			t.Errorf("%q: want unrestricted, got %v, %v", raw, p, err)
		}
	}

	core, err := ParseToolProfile("core")
	if err != nil {
		t.Fatal(err)
	}
	if len(core) != len(CoreTools) {
		t.Fatalf("core = %v, want exactly %v", core, CoreTools)
	}

	ext, err := ParseToolProfile(" core , ask_notes,get_fact ")
	if err != nil {
		t.Fatal(err)
	}
	if !ext["ask_notes"] || !ext["get_fact"] || !ext["remember"] || ext["forget"] {
		t.Fatalf("core plus extras parsed wrong: %v", ext)
	}

	// A typo must fail loudly, not drop the tool the person meant to keep.
	if _, err := ParseToolProfile("core,serch_notes"); err == nil ||
		!strings.Contains(err.Error(), "serch_notes") {
		t.Fatalf("unknown tool accepted: %v", err)
	}
	if _, err := ParseToolProfile(",,"); err == nil {
		t.Fatal("an empty selection must be an error, not an empty mount")
	}
}

// Every core tool must exist, or the lean profile silently ships short.
func TestCoreToolsAreAdvertised(t *testing.T) {
	known := map[string]bool{}
	for _, tl := range Tools() {
		known[tl.Name] = true
	}
	for _, name := range CoreTools {
		if !known[name] {
			t.Errorf("core tool %q is not an advertised tool", name)
		}
	}
}

func listed(t *testing.T, s *Server) []string {
	t.Helper()
	resp := s.handle(request{JSONRPC: "2.0", ID: []byte("1"), Method: "tools/list"})
	tools := resp.Result.(map[string]any)["tools"].([]tool)
	names := make([]string, len(tools))
	for i, tl := range tools {
		names[i] = tl.Name
	}
	return names
}

func TestProfileFiltersToolsList(t *testing.T) {
	s := New("http://127.0.0.1:1", "test")
	if got := len(listed(t, s)); got != len(Tools()) {
		t.Fatalf("no profile should list all %d tools, got %d", len(Tools()), got)
	}
	s.Profile, _ = ParseToolProfile("core")
	got := listed(t, s)
	if len(got) != len(CoreTools) {
		t.Fatalf("core profile listed %v", got)
	}
}

// The point of the profile is the context it saves; hold it to that.
func TestCoreProfileIsSmall(t *testing.T) {
	s := New("http://127.0.0.1:1", "test")
	full, _ := json.Marshal(s.advertised(Tools()))
	s.Profile, _ = ParseToolProfile("core")
	core, _ := json.Marshal(s.advertised(Tools()))
	if len(core)*4 > len(full) {
		t.Fatalf("core schema is %d bytes against %d for all tools; it should be under a quarter",
			len(core), len(full))
	}
}

// A hidden tool answers exactly like a tool that does not exist, and never
// reaches the API.
func TestHiddenToolIsRefused(t *testing.T) {
	hits := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	defer api.Close()

	s := New(api.URL, "test")
	s.Profile, _ = ParseToolProfile("core")
	call := func(name string) map[string]any {
		params, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		resp := s.handle(request{JSONRPC: "2.0", ID: []byte("2"), Method: "tools/call", Params: params})
		return resp.Result.(map[string]any)
	}

	hidden, missing := call("forget"), call("no_such_tool")
	if hidden["isError"] != true || missing["isError"] != true {
		t.Fatalf("hidden=%v missing=%v: both must be errors", hidden, missing)
	}
	text := func(m map[string]any) string {
		return m["content"].([]map[string]string)[0]["text"]
	}
	if text(hidden) != "unknown tool: forget" {
		t.Fatalf("hidden tool said %q", text(hidden))
	}
	if hits != 0 {
		t.Fatalf("a hidden tool reached the API %d time(s)", hits)
	}

	if r := call("get_briefing"); r["isError"] == true {
		t.Fatalf("a core tool was refused: %v", r)
	}
	if hits != 1 {
		t.Fatalf("core tool should reach the API once, got %d", hits)
	}
}
