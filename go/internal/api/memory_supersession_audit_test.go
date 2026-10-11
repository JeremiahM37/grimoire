package api

import (
	"net/http"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// Two true facts about two subjects that share a head noun. Today the rules
// strike one of them (the known defect, see internal/memory/contest.go). Under
// the `cascade` flag with no decision server, both stay live.
const (
	billingLag   = "the billing postgres replica lags 2 seconds"
	analyticsLag = "the analytics postgres replica lags 40 seconds"
)

func liveTexts(t *testing.T, h http.Handler) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, f := range recallFacts(t, h, "?q=postgres") {
		if s, ok := f["text"].(string); ok {
			out[s] = true
		}
	}
	return out
}

func writePair(t *testing.T, h http.Handler) {
	t.Helper()
	remember(t, h, map[string]any{"text": billingLag, "topic": "ops", "agent": "claude"})
	remember(t, h, map[string]any{"text": analyticsLag, "topic": "ops", "agent": "claude"})
}

func TestFlagOffRulesStillStrikeTheContestedFact(t *testing.T) {
	restore := exp.Set()
	defer restore()
	_, h := testServer(t)
	writePair(t, h)
	live := liveTexts(t, h)
	if live[billingLag] == live[analyticsLag] {
		t.Fatalf("flag off: expected exactly one of the pair live, got %v", live)
	}
}

func TestFlagOnKeepsBothWhenNoGateIsConfigured(t *testing.T) {
	restore := exp.Set("cascade")
	defer restore()
	_, h := testServer(t)
	writePair(t, h)
	live := liveTexts(t, h)
	if !live[billingLag] || !live[analyticsLag] {
		t.Fatalf("flag on, no gate: both facts must stay live, got %v", live)
	}
}

func TestSupersessionReviewListsAndRestores(t *testing.T) {
	restore := exp.Set()
	defer restore()
	_, h := testServer(t)
	writePair(t, h)

	w := do(t, h, "GET", "/api/memory/supersessions", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", w.Code, w.Body)
	}
	var list struct {
		Supersessions  []map[string]any `json:"supersessions"`
		Undecided      int              `json:"undecided"`
		GateConfigured bool             `json:"gate_configured"`
	}
	decode(t, w, &list)
	if len(list.Supersessions) != 1 {
		t.Fatalf("supersessions = %d, want 1: %v", len(list.Supersessions), list.Supersessions)
	}
	row := list.Supersessions[0]
	if row["judged_distinct"] != false || row["source"] != "fallback" || list.GateConfigured {
		t.Errorf("with no gate the row must be undecided, not judged distinct: %v", row)
	}
	id, _ := row["id"].(string)

	// An agent may not restore, and may not review.
	if w := do(t, h, "POST", "/api/memory/supersessions/restore?agent=claude",
		map[string]any{"id": id}); w.Code != http.StatusForbidden {
		t.Fatalf("agent restore = %d, want 403", w.Code)
	}
	if w := do(t, h, "GET", "/api/memory/supersessions?agent=claude", nil); w.Code != http.StatusForbidden {
		t.Fatalf("agent list = %d, want 403", w.Code)
	}

	w = do(t, h, "POST", "/api/memory/supersessions/restore", map[string]any{"id": id})
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", w.Code, w.Body)
	}
	if live := liveTexts(t, h); !live[row["old"].(string)] {
		t.Fatalf("restored fact is not recalled again: %v", live)
	}
	if w := do(t, h, "POST", "/api/memory/supersessions/restore", map[string]any{"id": id}); w.Code != http.StatusNotFound {
		t.Fatalf("restoring a live fact = %d, want 404", w.Code)
	}
}
