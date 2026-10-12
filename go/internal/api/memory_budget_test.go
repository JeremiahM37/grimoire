package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

// slowModel is an Ollama stand-in that answers after a long delay, or as soon as
// the caller goes away. A write with a short budget must not wait for it.
func slowModel(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"response":"ADD"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A write whose extraction call runs past the budget returns quickly, keeps the
// fact, and says the model was skipped.
func TestWriteBudgetCutsExtractionShortAndStoresTheFact(t *testing.T) {
	model := slowModel(t)
	t.Setenv("GRIMOIRE_WRITE_MODEL_BUDGET", "200ms")
	s, h := testServer(t)
	s.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": model.URL}, nil)

	text := "the deploy host moved to the other box last week for the cutover"
	start := time.Now()
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "text": text})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("write took %v with a 200ms budget", elapsed)
	}
	if out["model_skipped"] != "budget" {
		t.Fatalf("model_skipped = %v, want budget; response %v", out["model_skipped"], out)
	}
	if out["id"] == nil || !strings.Contains(noteBody(t, h, "memory/ops.md"), text) {
		t.Fatalf("the fact was not stored after the budget ran out:\n%s", noteBody(t, h, "memory/ops.md"))
	}
}

// The reconcile decision shares the same budget: a write that has an existing
// fact to reconcile against falls back to the rules when the model is slow.
func TestWriteBudgetCutsReconcileShortAndKeepsTheRuleDecision(t *testing.T) {
	model := slowModel(t)
	t.Setenv("GRIMOIRE_WRITE_MODEL_BUDGET", "200ms")
	s, h := testServer(t)
	rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "cache", "infer": false, "text": "the cache is redis"})
	s.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": model.URL}, nil)

	start := time.Now()
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "cache", "text": "the cache is memcached"})
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("write took %v with a 200ms budget", elapsed)
	}
	if out["model_skipped"] != "budget" {
		t.Fatalf("model_skipped = %v, want budget; response %v", out["model_skipped"], out)
	}
	if out["id"] == nil {
		t.Fatalf("no result for the write: %v", out)
	}
}

// Without a configured model nothing is skipped, whatever the budget.
func TestWriteBudgetIsNotReportedWithoutAModel(t *testing.T) {
	t.Setenv("GRIMOIRE_WRITE_MODEL_BUDGET", "200ms")
	_, h := testServer(t)
	out := rememberFrom(t, h, anchorLoopback, map[string]any{"topic": "ops", "text": "the gate is 0.01"})
	if out["model_skipped"] != nil {
		t.Fatalf("model_skipped reported with no model: %v", out)
	}
}

func TestWriteModelBudgetParsing(t *testing.T) {
	cases := map[string]time.Duration{
		"":      defaultWriteModelBudget,
		"0":     0,
		"5s":    5 * time.Second,
		"bogus": defaultWriteModelBudget,
		"-1s":   defaultWriteModelBudget,
	}
	for raw, want := range cases {
		t.Setenv("GRIMOIRE_WRITE_MODEL_BUDGET", raw)
		if got := writeModelBudget(); got != want {
			t.Errorf("GRIMOIRE_WRITE_MODEL_BUDGET=%q: %v, want %v", raw, got, want)
		}
	}
}
