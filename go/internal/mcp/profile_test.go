package mcp

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestParseToolProfile(t *testing.T) {
	// Unrestricted only when asked for by name.
	for _, raw := range []string{"all", "ALL", "core,all", "banks,all"} {
		p, err := ParseToolProfile(raw)
		if err != nil || p != nil {
			t.Errorf("%q: want unrestricted, got %v, %v", raw, p, err)
		}
	}

	// Unset or blank is the core profile, not everything.
	for _, raw := range []string{"", "  "} {
		p, err := ParseToolProfile(raw)
		if err != nil || len(p) != len(CoreTools) {
			t.Errorf("%q: want the core profile (%d tools), got %d, %v", raw, len(CoreTools), len(p), err)
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

	banks, err := ParseToolProfile("core,banks")
	if err != nil {
		t.Fatal(err)
	}
	if !banks["retain"] || !banks["get_briefing"] || banks["forget"] || len(banks) != len(CoreTools)+len(GroupTools("banks")) {
		t.Fatalf("core,banks parsed wrong: %d tools", len(banks))
	}

	// A typo must fail loudly, not drop the tool the person meant to keep.
	if _, err := ParseToolProfile("core,serch_notes"); err == nil ||
		!strings.Contains(err.Error(), "serch_notes") {
		t.Fatalf("unknown tool accepted: %v", err)
	}
	if _, err := ParseToolProfile("bankz"); err == nil || !strings.Contains(err.Error(), "bankz") {
		t.Fatalf("unknown group accepted: %v", err)
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

// A tool that is in neither core nor a named group is invisible to every
// profile except all. This is the test that fails when someone adds a tool and
// forgets where it belongs.
func TestEveryToolIsCoreOrInExactlyOneGroup(t *testing.T) {
	core := map[string]bool{}
	for _, c := range CoreTools {
		core[c] = true
	}
	seen := map[string]string{}
	for _, g := range toolGroups {
		for _, name := range g.tools {
			if core[name] {
				t.Errorf("%q is in core and in group %s; a core tool belongs to no group", name, g.name)
			}
			if prev, dup := seen[name]; dup {
				t.Errorf("%q is in both group %s and group %s", name, prev, g.name)
			}
			seen[name] = g.name
		}
	}
	var orphans []string
	for _, tl := range Tools() {
		if !core[tl.Name] && seen[tl.Name] == "" {
			orphans = append(orphans, tl.Name)
		}
	}
	if len(orphans) > 0 {
		t.Errorf("tools in no profile group (add them to toolGroups in profile.go): %v", orphans)
	}
	known := knownTools()
	for name := range seen {
		if !known[name] {
			t.Errorf("group %s names %q, which is not a tool", seen[name], name)
		}
	}
}

func TestGroupNamesAreSelectable(t *testing.T) {
	for _, g := range GroupNames() {
		if g == "core" || g == "all" {
			t.Errorf("group name %q collides with a keyword", g)
		}
		p, err := ParseToolProfile(g)
		if err != nil || len(p) != len(GroupTools(g)) {
			t.Errorf("group %s: %d tools, %v", g, len(p), err)
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
	s.Profile = nil
	if got := len(listed(t, s)); got != len(Tools()) {
		t.Fatalf("an unrestricted server should list all %d tools, got %d", len(Tools()), got)
	}
	s.Profile, _ = ParseToolProfile("core")
	got := listed(t, s)
	if len(got) != len(CoreTools) {
		t.Fatalf("core profile listed %v", got)
	}
}

// schemaBytes is the size of tools/list for a profile, as a client receives it.
func schemaBytes(t *testing.T, raw string) (count, size int) {
	t.Helper()
	s := New("http://127.0.0.1:1", "test")
	p, err := ParseToolProfile(raw)
	if err != nil {
		t.Fatal(err)
	}
	s.Profile = p
	b, _ := json.Marshal(s.advertised(Tools()))
	return len(s.advertised(Tools())), len(b)
}

// Measured on every profile so a regression shows in the test log; core is
// the one that is held to a limit, since it is what every session pays for.
func TestSchemaBytesPerProfile(t *testing.T) {
	for _, raw := range append([]string{"all", "core"}, GroupNames()...) {
		n, b := schemaBytes(t, raw)
		t.Logf("%-12s %3d tools %7d bytes", raw, n, b)
	}
	if _, b := schemaBytes(t, "core"); b >= 16*1024 {
		t.Fatalf("the core tools/list is %d bytes; it must stay under 16 KB", b)
	}
}

// Hidden tools are advertised less, not refused: a client or prompt that names
// a tool outside the profile still gets it, and reaches the API once.
func TestHiddenToolStillAnswers(t *testing.T) {
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

	if r := call("forget"); r["isError"] == true {
		t.Fatalf("a hidden but real tool was refused: %v", r)
	}
	if hits != 1 {
		t.Fatalf("hidden tool should reach the API once, got %d", hits)
	}

	missing := call("no_such_tool")
	if missing["isError"] != true {
		t.Fatalf("a name that is not a tool must be an error: %v", missing)
	}
	text := missing["content"].([]map[string]string)[0]["text"]
	if text != "unknown tool: no_such_tool" {
		t.Fatalf("unknown tool said %q", text)
	}
	if hits != 1 {
		t.Fatalf("an unknown tool reached the API")
	}
}

// The default mount is the core profile, and the server says more exists.
func TestDefaultProfileIsCoreAndInstructionsPointAtGroups(t *testing.T) {
	p, err := ParseToolProfile("")
	if err != nil || len(p) != len(CoreTools) {
		t.Fatalf("unset GRIMOIRE_MCP_TOOLS should be core: %d tools, %v", len(p), err)
	}
	if !strings.Contains(Instructions, "GRIMOIRE_MCP_TOOLS") || !strings.Contains(Instructions, "banks") {
		t.Fatalf("instructions do not say how to list more tools: %q", Instructions)
	}
}

// The counts stated in the docs are checked against the server, so they cannot
// drift the way "all 35" did.
var (
	docToolCount = regexp.MustCompile("`(all|core)` is (\\d+) tools, about (\\d+) KB")
)

func TestDocsStateTheRealToolCounts(t *testing.T) {
	root := repoRoot(t)
	wantAll, bytesAll := schemaBytes(t, "all")
	wantCore, bytesCore := schemaBytes(t, "core")
	want := map[string][2]int{
		"all":  {wantAll, int(math.Round(float64(bytesAll) / 1024))},
		"core": {wantCore, int(math.Round(float64(bytesCore) / 1024))},
	}
	for _, rel := range []string{"README.md", filepath.Join("docs", "CONFIG.md")} {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, m := range docToolCount.FindAllStringSubmatch(string(b), -1) {
			var count, kb int
			fmt.Sscan(m[2], &count)
			fmt.Sscan(m[3], &kb)
			found[m[1]] = true
			w := want[m[1]]
			if count != w[0] {
				t.Errorf("%s says %s is %d tools; the server has %d", rel, m[1], count, w[0])
			}
			if math.Abs(float64(kb-w[1])) > 1 {
				t.Errorf("%s says %s is about %d KB of schema; it is %d KB", rel, m[1], kb, w[1])
			}
		}
		for _, p := range []string{"all", "core"} {
			if !found[p] {
				t.Errorf("%s does not state the tool count for %s in the form %q", rel, p,
					"`all` is 85 tools, about 66 KB")
			}
		}
	}
}
