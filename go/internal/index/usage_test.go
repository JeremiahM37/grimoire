package index

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/usage"
)

func TestModelCallsRoundTrip(t *testing.T) {
	ix := testIndex(t)
	now := time.Now().UTC().Truncate(time.Second)
	calls := []usage.Call{
		{At: now.Add(-2 * time.Hour), Provider: usage.OpenAI, Model: "gpt-4o-mini",
			Surface: "ask", Agent: "claude-code", InputTokens: 1200, OutputTokens: 300,
			LatencyMS: 850, Cost: 0.00036, CostKnown: true},
		{At: now.Add(-time.Hour), Provider: usage.Ollama, Model: "qwen3.5:4b",
			Surface: "intent", InputTokens: 400, OutputTokens: 20, CostKnown: true},
		{At: now, Provider: usage.OpenRouter, Model: "some/model",
			Surface: "ask", InputTokens: 10, OutputTokens: 5, CostKnown: false,
			Error: "rate limited"},
	}
	for _, c := range calls {
		if err := ix.Record(c); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ix.ModelCalls(now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d calls, want 3", len(got))
	}
	// Newest first: a dashboard shows the most recent activity at the top.
	if got[0].Model != "some/model" {
		t.Errorf("ordering wrong; first is %q", got[0].Model)
	}
	if got[0].Error != "rate limited" {
		t.Errorf("error text lost: %q", got[0].Error)
	}
	// cost_known must survive as false, or an unpriced call reads as free.
	if got[0].CostKnown {
		t.Error("an unpriced call round-tripped as priced")
	}
	last := got[2]
	if last.Provider != usage.OpenAI || last.Surface != "ask" || last.Agent != "claude-code" {
		t.Errorf("attribution lost: %+v", last)
	}
	if last.InputTokens != 1200 || last.LatencyMS != 850 {
		t.Errorf("numbers lost: %+v", last)
	}
}

// The window must actually exclude, or "last 24h" silently reports all time.
func TestModelCallsRespectTheWindow(t *testing.T) {
	ix := testIndex(t)
	now := time.Now().UTC()
	_ = ix.Record(usage.Call{At: now.Add(-48 * time.Hour), Provider: usage.OpenAI, Model: "old"})
	_ = ix.Record(usage.Call{At: now, Provider: usage.OpenAI, Model: "new"})
	got, err := ix.ModelCalls(now.Add(-24*time.Hour), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Model != "new" {
		t.Fatalf("window not applied: %+v", got)
	}
}

// The ledger grows with every question asked; nobody reconciles a two-year-old
// bill, so it has to be prunable.
func TestPruneDropsOldCallsOnly(t *testing.T) {
	ix := testIndex(t)
	now := time.Now().UTC()
	_ = ix.Record(usage.Call{At: now.Add(-100 * 24 * time.Hour), Model: "ancient"})
	_ = ix.Record(usage.Call{At: now, Model: "recent"})
	if err := ix.PruneModelCalls(now.Add(-30 * 24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, _ := ix.ModelCalls(time.Time{}, 0)
	if len(got) != 1 || got[0].Model != "recent" {
		t.Fatalf("prune kept the wrong rows: %+v", got)
	}
}
