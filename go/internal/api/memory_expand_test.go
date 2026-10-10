package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// rawRecall returns the response body exactly as the server wrote it, so the
// comparisons below are byte comparisons rather than decoded equality.
func rawRecall(t *testing.T, h http.Handler, query string) []byte {
	t.Helper()
	w := do(t, h, "GET", "/api/memory"+query, nil)
	if w.Code >= 400 {
		t.Fatalf("recall %s = %d: %s", query, w.Code, w.Body)
	}
	return w.Body.Bytes()
}

func seedGraph(t *testing.T, h http.Handler) {
	t.Helper()
	setClock(t, "2026-08-10 09:00")
	remember(t, h, map[string]any{"topic": "team", "text": "Priya Sharma moved the strix box to Lane Office"})
	setClock(t, "2026-08-16 09:00")
	remember(t, h, map[string]any{"topic": "team", "text": "Priya Sharma owns the release checklist"})
}

func TestRecallWithoutExpansionIsByteIdentical(t *testing.T) {
	_, h := testServer(t)
	seedGraph(t, h)
	base := rawRecall(t, h, "?q=release+checklist&limit=5")
	for _, q := range []string{
		"?q=release+checklist&limit=5&expand=0&hops=0",
		"?q=release+checklist&limit=5&expand=false",
	} {
		if got := rawRecall(t, h, q); !bytes.Equal(base, got) {
			t.Errorf("%s differs from the default recall:\n%s\n%s", q, base, got)
		}
	}
	var facts []map[string]any
	if err := json.Unmarshal(base, &facts); err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		for _, k := range []string{"via", "variants", "hop", "connect", "fused"} {
			if _, ok := f[k]; ok {
				t.Errorf("default recall carries %q: %v", k, f)
			}
		}
	}
}

func TestRecallGraphHopsAddsAnExplainedNeighbour(t *testing.T) {
	_, h := testServer(t)
	seedGraph(t, h)
	raw := rawRecall(t, h, "?q=release+checklist&limit=1&hops=1&explain=1")
	var facts []map[string]any
	if err := json.Unmarshal(raw, &facts); err != nil {
		t.Fatal(err)
	}
	var graph map[string]any
	for _, f := range facts {
		if f["via"] == "graph" {
			graph = f
		}
	}
	if graph == nil {
		t.Fatalf("no graph-added fact in %s", raw)
	}
	if graph["connect"] != "priya sharma" || graph["hop"] != float64(1) {
		t.Errorf("graph fact = %v, want connect priya sharma, hop 1", graph)
	}
	if _, ok := graph["scores"]; !ok {
		t.Error("explain=1 did not return the score breakdown")
	}
}

func TestRecallExpandExplainsVariantsAndFusedScore(t *testing.T) {
	_, h := testServer(t)
	setClock(t, "2026-08-16 09:00")
	remember(t, h, map[string]any{"topic": "ops", "text": "deploy failed on staging host"})
	remember(t, h, map[string]any{"topic": "ops", "text": "proxy config lives in nginx"})
	remember(t, h, map[string]any{"topic": "ops", "text": "proxy certs renew monthly"})

	raw := rawRecall(t, h, "?q=deploying+proxy&limit=2&expand=1&explain=1")
	var facts []map[string]any
	if err := json.Unmarshal(raw, &facts); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range facts {
		if f["text"] != "deploy failed on staging host" {
			continue
		}
		found = true
		vs, _ := f["variants"].([]any)
		if len(vs) == 0 {
			t.Errorf("expanded fact lists no variants: %v", f)
		}
		if _, ok := f["fused"].(float64); !ok {
			t.Errorf("explain did not report the fused score: %v", f)
		}
		if f["via"] != nil {
			t.Errorf("a direct hit is marked via %v", f["via"])
		}
	}
	if !found {
		t.Errorf("expansion did not surface the stem fact: %s", raw)
	}
}

func TestRecallExpandDefaultFollowsTheEnvironment(t *testing.T) {
	_, h := testServer(t)
	seedGraph(t, h)
	t.Setenv(recallExpandEnv, "1")
	on := rawRecall(t, h, "?q=Priya&limit=5")
	if !bytes.Contains(on, []byte(`"variants"`)) {
		t.Errorf("GRIMOIRE_RECALL_EXPAND=1 did not expand by default: %s", on)
	}
	// An explicit expand=0 beats the environment.
	off := rawRecall(t, h, "?q=Priya&limit=5&expand=0")
	if bytes.Contains(off, []byte(`"variants"`)) {
		t.Errorf("expand=0 did not override the environment: %s", off)
	}
}

func TestRecallRefusesMalformedHops(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	for _, q := range []string{"?q=x&hops=3", "?q=x&hops=-1", "?q=x&hops=two"} {
		w := do(t, h, "GET", "/api/memory"+q, nil)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", q, w.Code)
		}
	}
}
