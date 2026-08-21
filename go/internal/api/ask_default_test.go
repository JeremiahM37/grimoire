package api

// The ask-path default lives in its own file so it can be reverted on its own.
// Decomposition was the default until it was measured: plain retrieval scored
// 49.0% against 47.1% (4B decomposer) and 45.1% (36B) on the LongMemEval
// category the mechanism exists for, at ~70x the retrieval latency and one
// model call a question -- and every published benchmark number was measured
// on the plain path, so the benchmarks did not describe what a user got.
// See benchmarks/longmemeval/REPORT-multihop.md.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

type stubSettings map[string]string

func (s stubSettings) Get(k string) string { return s[k] }

func TestAskDoesNotDecomposeUnlessAsked(t *testing.T) {
	var mu sync.Mutex
	var prompts []string
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Prompt string `json:"prompt"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		prompts = append(prompts, in.Prompt)
		mu.Unlock()
		// One line back: enough for Decompose to parse a sub-question, for
		// Rerank to name a candidate, and for the answer path to use it.
		_, _ = w.Write([]byte(`{"response":"1"}`))
	}))
	defer llm.Close()

	s, h := testServer(t)
	s.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": llm.URL}, nil)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "runbook.md",
		"body": "# Runbook\n\nthe kestrel deploy host is prod-1 and the gateway restarts nightly"})

	calls := func(body map[string]any) int {
		mu.Lock()
		prompts = nil
		mu.Unlock()
		if w := do(t, h, "POST", "/api/ask", body); w.Code != http.StatusOK {
			t.Fatalf("ask %v = %d: %s", body, w.Code, w.Body)
		}
		mu.Lock()
		defer mu.Unlock()
		return len(prompts)
	}

	q := "where does the kestrel deploy host live and when does the gateway restart"
	base := calls(map[string]any{"q": q})
	off := calls(map[string]any{"q": q, "smart": false})
	on := calls(map[string]any{"q": q, "smart": true})

	if base != off {
		t.Errorf("the default made %d model calls and smart:false made %d — "+
			"the default is not off", base, off)
	}
	if on <= base {
		t.Errorf("smart:true made %d model calls and the default %d — the flag "+
			"is being ignored, so this test would pass on any default", on, base)
	}
	t.Logf("model calls: default=%d smart:false=%d smart:true=%d", base, off, on)
}
