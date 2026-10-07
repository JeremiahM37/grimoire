package bank

import (
	"context"
	"strings"
	"testing"
)

func TestDeltaRefreshEditsOnlyModelSectionsAndHoldsPersonEdits(t *testing.T) {
	h := newHarness(t, true)
	answer := "## Home\n\nAlice lives in Lyon.\n\n## Work\n\nAlice is a baker."
	h.llm.route = func(sys, user string) (string, string) {
		if strings.HasPrefix(sys, deltaSystem[:40]) {
			return js(map[string]any{"edits": []map[string]any{
				{"op": "append", "section": "Home", "text": "Update: moved again."},
				{"op": "append", "section": "Work", "text": "Update: now a chef."},
			}}), "stop"
		}
		return refreshRouter(&answer)(sys, user)
	}
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	m, err := h.e.CreateModel("b", ModelSpec{Name: strPtr("Alice"), Question: strPtr("Who is Alice?")})
	if err != nil {
		t.Fatal(err)
	}
	if out, err := h.e.RefreshModel(context.Background(), "b", m.ID); err != nil || out.Outcome != "written" {
		t.Fatalf("full refresh = %+v %v", out, err)
	}
	mode := "delta"
	if _, err := h.e.UpdateModel("b", m.ID, ModelSpec{RefreshMode: &mode}); err != nil {
		t.Fatal(err)
	}
	// Nothing new: delta leaves it alone.
	if out, _ := h.e.RefreshModel(context.Background(), "b", m.ID); out.Outcome != "unchanged" || out.Mode != "delta" {
		t.Fatalf("no new facts = %+v", out)
	}
	// A person rewrites the Work section; then a new fact arrives.
	h.editFile(t, m.Path, "Alice is a baker.", "Alice is a baker (my words).")
	h.retain(t, "b", Item{Content: "Alice: I also took up pottery.", DocumentID: "d2"})
	out, err := h.e.RefreshModel(context.Background(), "b", m.ID)
	if err != nil || out.Outcome != "delta" || out.Held != 1 {
		t.Fatalf("delta = %+v %v", out, err)
	}
	got, _ := h.e.GetModel("b", m.ID)
	if !strings.Contains(got.Body, "moved again") || !strings.Contains(got.Body, "baker (my words).") ||
		strings.Contains(strings.Split(got.Body, "## Work")[1], "chef") {
		t.Fatalf("body = %q", got.Body)
	}
	if got.Authority != "human" || got.Proposal == nil || !strings.Contains(got.Proposal.Body, "now a chef") ||
		!strings.Contains(got.Proposal.Body, "my words") {
		t.Fatalf("person section must stay theirs and the held edit wait: %+v", got)
	}
	// Survives a rebuild from the files.
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	if g, _ := h.e.GetModel("b", m.ID); g.Authority != "human" || !strings.Contains(g.Body, "my words") {
		t.Fatalf("after reindex: %+v", g)
	}
	// The explicit full mode still proposes over the person's text.
	answer = "Alice rewritten."
	if out, _ := h.e.RefreshModelWith(context.Background(), "b", m.ID, RefreshOpts{Mode: "full"}); out.Outcome != "proposed" {
		t.Fatalf("full = %+v", out)
	}
}

func TestDeltaRefreshRuleFallbackWithoutModel(t *testing.T) {
	h := newHarness(t, true)
	answer := "## Home\n\nAlice lives in Lyon."
	h.llm.route = refreshRouter(&answer)
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	m, _ := h.e.CreateModel("b", ModelSpec{Name: strPtr("Alice"), Question: strPtr("Who is Alice?"), RefreshMode: strPtr("delta")})
	h.e.RefreshModel(context.Background(), "b", m.ID)
	h.retain(t, "b", Item{Content: "Alice: I got a cat.", DocumentID: "d2"})
	h.llm.route = func(sys, user string) (string, string) { return "not json", "stop" }
	out, err := h.e.RefreshModel(context.Background(), "b", m.ID)
	if err != nil || out.Outcome != "delta" {
		t.Fatalf("fallback = %+v %v", out, err)
	}
	got, _ := h.e.GetModel("b", m.ID)
	if !strings.Contains(got.Body, "## Recent additions") || !strings.Contains(got.Body, "Lyon.") {
		t.Fatalf("body = %q", got.Body)
	}
}
