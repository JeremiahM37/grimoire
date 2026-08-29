package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

// One Client is shared by every request the server handles, so the caller's
// identity cannot live on it. Attributing a call to the wrong surface is a
// quiet failure — the total stays right, the rollup that makes it actionable
// does not, and nothing anywhere reports an error. Run with -race this also
// catches the write itself rather than only its consequences.

type syncCapture struct {
	mu    sync.Mutex
	calls []usage.Call
}

func (s *syncCapture) Record(c usage.Call) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, c)
	return nil
}

func TestConcurrentSurfacesDoNotCrossAttribute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"response":          "ok",
			"prompt_eval_count": 10,
			"eval_count":        2,
		})
	}))
	defer srv.Close()

	cap := &syncCapture{}
	shared := New(mapSettings(map[string]string{
		"llm": "ollama", "ollama_url": srv.URL, "llm_model": "qwen3.5:4b",
	}), nil)
	shared.HTTP = srv.Client()
	shared.Usage = usage.NewRecorder(cap)

	// Each pairing is unique, so any row whose surface and agent do not match
	// is a call one goroutine attributed to another's caller.
	surfaces := []struct{ surface, agent string }{
		{"ask", "claude-code"},
		{"rerank", "codex"},
		{"intent", "cursor"},
		{"summarize", "zed"},
		{"embed", "continue"},
	}
	const each = 20

	var wg sync.WaitGroup
	for _, s := range surfaces {
		for i := 0; i < each; i++ {
			wg.Add(1)
			go func(surface, agent string) {
				defer wg.Done()
				if _, err := shared.WithSurface(surface, agent).Complete("q", "ollama"); err != nil {
					t.Error(err)
				}
			}(s.surface, s.agent)
		}
	}
	wg.Wait()

	if shared.surface != "" || shared.agent != "" {
		t.Errorf("the shared client was mutated: surface=%q agent=%q — WithSurface must copy",
			shared.surface, shared.agent)
	}

	expect := map[string]string{}
	for _, s := range surfaces {
		expect[s.surface] = s.agent
	}
	counts := map[string]int{}
	cap.mu.Lock()
	defer cap.mu.Unlock()
	if len(cap.calls) != len(surfaces)*each {
		t.Fatalf("recorded %d calls, want %d", len(cap.calls), len(surfaces)*each)
	}
	for _, c := range cap.calls {
		want, ok := expect[c.Surface]
		if !ok {
			t.Fatalf("call recorded against an unknown surface %q", c.Surface)
		}
		if c.Agent != want {
			t.Fatalf("surface %q was attributed to agent %q, want %q — a client field written "+
				"per call reports whoever raced in last", c.Surface, c.Agent, want)
		}
		counts[c.Surface]++
	}
	for s, n := range counts {
		if n != each {
			t.Errorf("surface %q recorded %d calls, want %d", s, n, each)
		}
	}
}

// A copy must not share the pieces that make the call, or a per-request client
// would answer against the wrong provider.
func TestWithSurfaceCopiesOnlyTheCaller(t *testing.T) {
	c := New(mapSettings(map[string]string{"llm": "ollama", "llm_model": "m"}), nil)
	cap := &syncCapture{}
	c.Usage = usage.NewRecorder(cap)

	cp := c.WithSurface("ask", "claude-code")
	if cp == c {
		t.Fatal("WithSurface returned the same client; a shared field would be written per call")
	}
	if cp.Settings == nil || cp.HTTP != c.HTTP || cp.Usage != c.Usage {
		t.Error("the copy lost settings, transport or recorder it needs to make and book a call")
	}
	if cp.surface != "ask" || cp.agent != "claude-code" {
		t.Errorf("copy carries surface=%q agent=%q", cp.surface, cp.agent)
	}
	// Copying a copy re-labels rather than compounding.
	again := cp.WithSurface("rerank", "codex")
	if again.surface != "rerank" || again.agent != "codex" {
		t.Error("re-labelling a copy must replace the caller, not keep the first one")
	}
	if cp.surface != "ask" {
		t.Error("re-labelling mutated the client it was copied from")
	}
}

func TestWithSurfaceOnANilClientIsSafe(t *testing.T) {
	var c *Client
	if c.WithSurface("ask", "x") != nil {
		t.Error("a nil client must copy to nil rather than panicking at a call site that has no AI configured")
	}
}
