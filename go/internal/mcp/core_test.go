package mcp

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func resetCore() {
	core.Lock()
	core.fetched = time.Time{}
	core.Unlock()
}

func initInstructions(t *testing.T, s *Server) string {
	t.Helper()
	resetCore()
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	inst, _ := resps[0]["result"].(map[string]any)["instructions"].(string)
	return inst
}

func TestInstructionsCarryTheMemoryCore(t *testing.T) {
	var asked string
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"core":"- Never push unasked (m:3e99)\n- Recall 'deploy' for release steps"}`))
	})
	inst := initInstructions(t, s)
	if !strings.HasPrefix(inst, Instructions) || !strings.Contains(inst, "Never push unasked") {
		t.Fatalf("instructions = %q", inst)
	}
	if asked != "/api/memory/core?budget=6000" {
		t.Errorf("asked %q", asked)
	}
}

func TestInstructionsAreUnchangedWithoutACoreEndpoint(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	if inst := initInstructions(t, s); inst != Instructions {
		t.Fatalf("a 404 must leave the instructions as they were: %q", inst)
	}
	s = stubAPI(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"error":"x"}`)) })
	if inst := initInstructions(t, s); inst != Instructions {
		t.Fatalf("an unrecognised reply must be ignored: %q", inst)
	}
}

func TestMemoryCoreIsBoundedAndCanBeTurnedOff(t *testing.T) {
	long := strings.Repeat("- a standing rule that goes on and on\n", 400)
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(long)) })
	inst := initInstructions(t, s)
	if got := len(inst) - len(Instructions) - len(coreHeading); got > coreMaxBytes || got < coreMaxBytes/2 {
		t.Errorf("core is %d bytes, want at most %d", got, coreMaxBytes)
	}
	t.Setenv(EnvCore, "0")
	if inst := initInstructions(t, s); inst != Instructions {
		t.Error("GRIMOIRE_MCP_CORE=0 must leave the instructions alone")
	}
}

func TestBoundCoreNeverSplitsACharacter(t *testing.T) {
	got := boundCore(strings.Repeat("é", 100), 51)
	if len(got) > 51 || strings.ContainsRune(got, '�') {
		t.Errorf("%q", got)
	}
}
