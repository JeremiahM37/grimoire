package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

func TestKnowledgeExtractionIsExplicitCachedAndScoped(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"response": `[{"subject":"Kestrel","relation":"uses","object":"Copper","quote":"Kestrel uses Copper"}]`})
	}))
	defer model.Close()
	server, handler := testServer(t)
	server.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": model.URL}, nil)
	writeSecurityNote(t, server, "public.md", "Kestrel uses Copper", nil)
	writeSecurityNote(t, server, "hidden.md", "Kestrel uses Copper", map[string]any{"private": true})
	graph := do(t, handler, "GET", "/api/knowledge/graph", nil)
	if graph.Code != http.StatusOK || calls.Load() != 0 {
		t.Fatalf("graph unexpectedly invoked model: %d, %d calls", graph.Code, calls.Load())
	}
	for _, status := range []string{"indexed", "cached"} {
		response := do(t, handler, "POST", "/api/knowledge/extract", map[string]any{"paths": []string{"public.md", "hidden.md"}})
		if response.Code != http.StatusOK {
			t.Fatalf("extract = %d %s", response.Code, response.Body)
		}
		var result struct {
			Results []struct {
				Status  string `json:"status"`
				Triples int    `json:"triples"`
			} `json:"results"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if len(result.Results) != 2 || result.Results[0].Status != status || result.Results[0].Triples != 1 || result.Results[1].Status != "error" {
			t.Fatalf("unexpected extraction result: %s", response.Body)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cached/private requests invoked model: %d calls", calls.Load())
	}
	graph = do(t, handler, "GET", "/api/knowledge/graph", nil)
	if graph.Code != http.StatusOK || !strings.Contains(graph.Body.String(), `"relation":"uses"`) || calls.Load() != 1 {
		t.Fatalf("semantic graph = %d %s (%d model calls)", graph.Code, graph.Body, calls.Load())
	}
}

func TestKnowledgeExtractionRejectsInvalidBatches(t *testing.T) {
	_, handler := testServer(t)
	for _, paths := range [][]string{nil, {""}, {"../outside.md"}, make([]string, 11)} {
		response := do(t, handler, "POST", "/api/knowledge/extract", map[string]any{"paths": paths})
		if response.Code != http.StatusBadRequest {
			t.Fatalf("paths %v = %d %s", paths, response.Code, response.Body)
		}
	}
}

func TestKnowledgeQueryRejectsInvalidDatesAndDepth(t *testing.T) {
	_, handler := testServer(t)
	for _, body := range []map[string]any{
		{"question": "launch", "after": "2026-02-30"},
		{"question": "launch", "before": "yesterday"},
		{"question": "launch", "after": "2026-03-01", "before": "2026-02-01"},
		{"question": "launch", "depth": -1},
	} {
		response := do(t, handler, "POST", "/api/knowledge/query", body)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %v = %d %s", body, response.Code, response.Body)
		}
	}
}
