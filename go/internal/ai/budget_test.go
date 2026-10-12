package ai

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A copy bound to a context stops its model call when the context ends, so the
// write budget can cut a hung model short.
func TestWithContextCancelsTheModelCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		_, _ = w.Write([]byte(`{"response":"late"}`))
	}))
	defer srv.Close()
	c := New(stubSettings{"llm": "ollama", "ollama_url": srv.URL}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.WithContext(ctx).Complete("hello", "ollama")
	if err == nil {
		t.Fatal("a call bound to an expired context succeeded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("call took %v; the context did not bound it", elapsed)
	}
	// The original client is untouched: it has no context of its own.
	if c.ctx != nil {
		t.Fatal("WithContext changed the shared client")
	}
}

type stubSettings map[string]string

func (s stubSettings) Get(k string) string { return s[k] }
