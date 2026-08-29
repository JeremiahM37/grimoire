package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

func seedCalls(t *testing.T, s *Server) {
	t.Helper()
	now := time.Now().UTC()
	for _, c := range []usage.Call{
		{At: now.Add(-time.Hour), Provider: usage.Anthropic, Model: "claude-sonnet-5",
			Surface: "ask", Agent: "claude-code", InputTokens: 100_000, OutputTokens: 10_000,
			Cost: 0.45, CostKnown: true},
		{At: now.Add(-30 * time.Minute), Provider: usage.Ollama, Model: "qwen3.5:4b",
			Surface: "intent", Agent: "claude-code", InputTokens: 500, OutputTokens: 50,
			CostKnown: true},
		{At: now.Add(-10 * time.Minute), Provider: usage.OpenRouter, Model: "some/model",
			Surface: "rerank", InputTokens: 2000, OutputTokens: 100, CostKnown: false},
	} {
		if err := s.Index.Record(c); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUsageReportsRollupsAndScope(t *testing.T) {
	s, h := testServer(t)
	seedCalls(t, s)

	w := do(t, h, "GET", "/api/usage?since=24h", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("usage = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Window  string        `json:"window"`
		Scope   string        `json:"scope"`
		Summary usage.Summary `json:"summary"`
		Recent  []usage.Call  `json:"recent"`
	}
	decode(t, w, &out)

	if out.Window != "24h" {
		t.Errorf("window = %q", out.Window)
	}
	if out.Summary.Calls != 3 {
		t.Errorf("calls = %d, want 3", out.Summary.Calls)
	}
	if out.Summary.InputTokens != 102_500 {
		t.Errorf("input tokens = %d, want 102500", out.Summary.InputTokens)
	}
	// The unpriced call must be counted, so the total reads as "at least this".
	if out.Summary.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1 — a routed model has no price on file",
			out.Summary.Unpriced)
	}
	if out.Summary.PricesUpdated == "" {
		t.Error("no price date reported; a cost with no date is a guess")
	}
	// The scope caveat travels with the payload, not only the docs. A client
	// rendering the number without it reports something untrue.
	if out.Scope == "" {
		t.Error("no scope statement in the response")
	}
	if len(out.Recent) != 3 {
		t.Errorf("recent = %d rows", len(out.Recent))
	}
}

func TestUsageRollsUpByProviderSurfaceAndAgent(t *testing.T) {
	s, h := testServer(t)
	seedCalls(t, s)
	var out struct {
		Summary usage.Summary `json:"summary"`
	}
	decode(t, do(t, h, "GET", "/api/usage", nil), &out)

	find := func(rows []usage.GroupTotal, key string) *usage.GroupTotal {
		for i := range rows {
			if rows[i].Key == key {
				return &rows[i]
			}
		}
		return nil
	}
	if g := find(out.Summary.ByProvider, "anthropic"); g == nil || g.Calls != 1 {
		t.Errorf("anthropic rollup: %+v", g)
	}
	if g := find(out.Summary.BySurface, "ask"); g == nil || g.Cost != 0.45 {
		t.Errorf("ask surface rollup: %+v", g)
	}
	if g := find(out.Summary.ByAgent, "claude-code"); g == nil || g.Calls != 2 {
		t.Errorf("agent rollup: %+v", g)
	}
	// A call with no agent must be visible, not dropped.
	if g := find(out.Summary.ByAgent, "(none)"); g == nil {
		t.Error("unattributed calls vanished from the agent rollup")
	}
	// Most expensive first, so the row worth reading is the first one.
	if len(out.Summary.ByProvider) > 1 && out.Summary.ByProvider[0].Key != "anthropic" {
		t.Errorf("rollup not sorted by cost: %+v", out.Summary.ByProvider)
	}
}

func TestAgentActivityReportsWhatAgentsDid(t *testing.T) {
	s, h := testServer(t)
	seedCalls(t, s)
	if _, err := s.WriteNote("memory/ops.md",
		"# Memory\n\n- **2026-08-29 09:00 · claude-code** — the deploy host is prod <!--m id=aaaaaaaaaaaa-->\n",
		map[string]any{"memory": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	w := do(t, h, "GET", "/api/usage/agents", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("agents = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Agents []struct {
			Agent      string  `json:"agent"`
			Facts      int     `json:"facts"`
			Calls      int     `json:"model_calls"`
			Cost       float64 `json:"model_cost"`
			Challenges int     `json:"challenges"`
		} `json:"agents"`
		Scope string `json:"scope"`
	}
	decode(t, w, &out)
	if len(out.Agents) == 0 {
		t.Fatal("no agents reported")
	}
	var cc *struct {
		Agent      string  `json:"agent"`
		Facts      int     `json:"facts"`
		Calls      int     `json:"model_calls"`
		Cost       float64 `json:"model_cost"`
		Challenges int     `json:"challenges"`
	}
	for i := range out.Agents {
		if out.Agents[i].Agent == "claude-code" {
			cc = &out.Agents[i]
		}
	}
	if cc == nil {
		t.Fatalf("claude-code missing: %+v", out.Agents)
	}
	if cc.Facts != 1 {
		t.Errorf("facts = %d, want 1", cc.Facts)
	}
	if cc.Calls != 2 {
		t.Errorf("model calls = %d, want 2", cc.Calls)
	}
	if out.Scope == "" {
		t.Error("no scope statement")
	}
}

// An empty ledger must answer, not error — a fresh install has no usage.
func TestUsageOnAnEmptyLedger(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "GET", "/api/usage", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("usage on an empty ledger = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Summary usage.Summary `json:"summary"`
	}
	decode(t, w, &out)
	if out.Summary.Calls != 0 || out.Summary.Cost != 0 {
		t.Errorf("empty ledger reported %+v", out.Summary)
	}
}
