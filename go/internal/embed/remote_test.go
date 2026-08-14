package embed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOllamaBatchAndFallback(t *testing.T) {
	var hits []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		switch r.URL.Path {
		case "/api/embed":
			// pretend this server is too old for the batch endpoint
			http.Error(w, "not found", http.StatusNotFound)
		case "/api/embeddings":
			json.NewEncoder(w).Encode(map[string]any{"embedding": []float32{1, 2, 3}})
		}
	}))
	defer ts.Close()

	o := NewOllama(ts.URL, "nomic-embed-text")
	if o.Signature() != "ollama:nomic-embed-text" {
		t.Errorf("signature = %q", o.Signature())
	}
	got := o.Embed([]string{"a", "b"})
	if len(got) != 2 || len(got[0]) != 3 {
		t.Fatalf("got %v", got)
	}
	// the batch endpoint must be tried FIRST — per-chunk round-trips crawl on a
	// large vault
	if hits[0] != "/api/embed" {
		t.Errorf("first call was %s, want the batch endpoint", hits[0])
	}
}

func TestOllamaBatchPreferred(t *testing.T) {
	calls := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		json.NewEncoder(w).Encode(map[string]any{
			"embeddings": [][]float32{{1, 0}, {0, 1}, {1, 1}}})
	}))
	defer ts.Close()
	got := NewOllama(ts.URL, "m").Embed([]string{"a", "b", "c"})
	if len(got) != 3 {
		t.Fatalf("got %d vectors", len(got))
	}
	if calls != 1 {
		t.Errorf("made %d HTTP calls for a 3-text batch, want 1", calls)
	}
}

func TestOpenAICompatibleEmbedder(t *testing.T) {
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.5, 0.5}}}})
	}))
	defer ts.Close()
	got := NewOpenAI(ts.URL, "sk-test", "text-embed").Embed([]string{"a"})
	if len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("got %v", got)
	}
	if auth != "Bearer sk-test" {
		t.Errorf("auth header = %q", auth)
	}
}

// An AI outage must degrade retrieval, never break indexing.
func TestChainFallsBackToHashOnFailure(t *testing.T) {
	dead := NewOllama("http://127.0.0.1:1", "m")
	chain := &Chain{Backends: []Backend{dead, Hash{}}}
	got := chain.Embed([]string{"alpha beta", "gamma"})
	if len(got) != 2 || len(got[0]) != Dim {
		t.Fatalf("chain did not fall back: %d vectors", len(got))
	}
	// the signature reports the PREFERRED backend, which is what the
	// embed-signature check uses to decide whether a re-embed is needed
	if chain.Signature() != "ollama:m" {
		t.Errorf("signature = %q", chain.Signature())
	}
}

func TestChainPrefersFirstWorkingBackend(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{{9, 9}}})
	}))
	defer ts.Close()
	chain := &Chain{Backends: []Backend{NewOllama(ts.URL, "m"), Hash{}}}
	got := chain.Embed([]string{"x"})
	if len(got[0]) != 2 || got[0][0] != 9 {
		t.Errorf("chain did not use the preferred backend: %v", got[0])
	}
}
