package api

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

// The MCP server asks this handler for the core and appends what it gets to
// its instructions. The two live in different packages and used to be tested
// only against a hand-written stub; this runs the real handler's reply through
// the real MCP initialize, so a rename of the "core" field cannot pass quietly.
func TestMCPInstructionsCarryWhatTheCoreHandlerReturns(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	dir := t.TempDir()
	for _, n := range []string{"alpha", "beta"} {
		storeNote(t, dir, "feedback_"+n+".md", "feedback", "never do "+n+" without asking", "Why: x. How to apply: y.")
	}
	storeNote(t, dir, "project_a.md", "project", "deploy thing one", "x")
	storeNote(t, dir, "project_b.md", "project", "deploy thing two", "x")
	if err := s.Settings.Update(map[string]string{"memory_canonical_dir": dir}); err != nil {
		t.Fatal(err)
	}

	var reply map[string]any
	decode(t, do(t, h, "GET", "/api/memory/core?budget=6000", nil), &reply)
	text, _ := reply["core"].(string)
	if !strings.Contains(text, "never do alpha") {
		t.Fatalf("handler reply has no rule text: %v", reply)
	}

	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	srv := mcp.New(ts.URL, "test-agent")
	var out bytes.Buffer
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}` + "\n")
	if err := srv.Serve(in, &out); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &frame); err != nil {
		t.Fatal(err)
	}
	inst := frame.Result.Instructions
	for _, want := range []string{"never do alpha", "never do beta"} {
		if !strings.Contains(inst, want) {
			t.Errorf("instructions lack %q:\n%s", want, inst)
		}
	}
}
