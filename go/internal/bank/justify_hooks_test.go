package bank

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
)

// consolidateLyon retains the Lyon/chess conversation and consolidates it, so
// one observation ("Alice is based in Lyon") rests on the Lyon fact.
func consolidateLyon(t *testing.T) (*harness, string) {
	t.Helper()
	h := newHarness(t, true)
	cons := &consolidator{plan: func(ids, texts []string, _ []map[string]any) map[string]any {
		var creates []any
		for i, id := range ids {
			if strings.Contains(texts[i], "Lyon") {
				creates = append(creates, map[string]any{"text": "Alice is based in Lyon", "source_fact_ids": []any{id},
					"evidence": []any{map[string]any{"fact_id": id, "quote": "lives in Lyon"}}, "reason": "new"})
			}
		}
		return map[string]any{"creates": creates, "updates": []any{}, "deletes": []any{}}
	}}
	h.llm.route = cons.route(extractOnly(livesIn("Lyon")))
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon and play chess.", DocumentID: "d1", Tags: []string{"fam"}})
	if _, err := h.e.Consolidate(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	items, _, _, err := h.e.ListObservations("b", ObservationQuery{})
	if err != nil || len(items) != 1 {
		t.Fatalf("observations = %+v err=%v", items, err)
	}
	return h, items[0].ID
}

func lyon(s string) bool { return strings.Contains(s, "Lyon") }

// With the flag off the store is never created and no output gains a field.
func TestJustifyFlagOffLeavesNoStore(t *testing.T) {
	h, _ := consolidateLyon(t)
	n, err := h.e.Index.DB.Count("SELECT COUNT(*) FROM sqlite_master WHERE name = 'justify_node'")
	if err != nil || n != 0 {
		t.Fatalf("justify table exists with the flag off: n=%d err=%v", n, err)
	}
	items, _, _, _ := h.e.ListObservations("b", ObservationQuery{})
	raw, _ := json.Marshal(items)
	if strings.Contains(string(raw), "stale") {
		t.Fatalf("flag-off listing carries stale fields: %s", raw)
	}
	acts, err := h.e.CascadeForget("b", lyon, func(s string) string { return "[forgotten]" }, CascadeOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range acts {
		if a.Kind == "justification" {
			t.Fatalf("flag-off cascade reported a justification: %+v", a)
		}
	}
}

// With the flag on, the observation's support is recorded, a dry-run forget
// reports it as a dependent the store loses without writing, and a real forget
// records the loss.
func TestJustifyCascadeReportsStoreDependents(t *testing.T) {
	restore := exp.Set(justifyFlag)
	defer restore()
	h, obsID := consolidateLyon(t)

	node := unitNode("b", obsID)
	st, err := h.e.justifyStore()
	if err != nil {
		t.Fatal(err)
	}
	if n, ok, err := st.Get(node); err != nil || !ok || !n.Valid {
		t.Fatalf("observation %s not recorded valid: %+v ok=%v err=%v", obsID, n, ok, err)
	}

	redact := func(s string) string { return "[forgotten]" }
	dry, err := h.e.CascadeForget("b", lyon, redact, CascadeOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, a := range dry {
		if a.Kind == "justification" && a.Ref == obsID && a.Action == ActJustifyLost {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("dry run did not report the observation as lost: %+v", dry)
	}
	if n, _, _ := st.Get(node); !n.Valid {
		t.Fatal("dry run changed the store")
	}

	if _, err := h.e.CascadeForget("b", lyon, redact, CascadeOptions{}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := st.Get(node); n.Valid {
		t.Fatal("forget did not record the observation as lost")
	}
	// The cascade removed the observation outright, so the store has nothing
	// left to report about it on a second pass.
	again, err := h.e.CascadeForget("b", lyon, redact, CascadeOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range again {
		if a.Kind == "justification" {
			t.Fatalf("second forget reported a dependent that no longer exists: %+v", a)
		}
	}
}
