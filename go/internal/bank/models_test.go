package bank

import (
	"context"
	"strings"
	"sync"
	"testing"
)

type snaps struct {
	mu   sync.Mutex
	rels []string
}

func (s *snaps) Snapshot(rel, _ string) {
	s.mu.Lock()
	s.rels = append(s.rels, rel)
	s.mu.Unlock()
}

func (s *snaps) count(rel string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.rels {
		if r == rel {
			n++
		}
	}
	return n
}

func refreshRouter(answer *string) func(string, string) (string, string) {
	var mu sync.Mutex
	return func(sys, user string) (string, string) {
		if strings.HasPrefix(sys, reflectMarker) {
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(user, "Call recall next") || strings.Contains(user, "Call search_") {
				return js(map[string]any{"tool": "recall", "args": map[string]any{"query": "Alice"}}), "stop"
			}
			return js(map[string]any{"tool": "done", "args": map[string]any{"answer": *answer}}), "stop"
		}
		return livesIn("Lyon")(user)
	}
}

func TestMentalModelRefreshNeverOverwritesAPersonsEdit(t *testing.T) {
	h := newHarness(t, true)
	hist := &snaps{}
	h.e.History = hist
	answer := "Alice lives in Lyon."
	h.llm.route = refreshRouter(&answer)
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon.", DocumentID: "d1"})
	m, err := h.e.CreateModel("b", ModelSpec{Name: strPtr("Where Alice lives"), Question: strPtr("Where does Alice live?"),
		Folder: strPtr("people")})
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "people/where-alice-lives" || m.Path != ModelPath("b", "people/where-alice-lives") {
		t.Fatalf("model = %+v", m)
	}
	got, _ := h.e.GetModel("b", m.ID)
	if !got.IsStale || got.StaleReason != "never_refreshed" {
		t.Errorf("a new model is stale: %+v", got)
	}
	out, err := h.e.RefreshModel(context.Background(), "b", m.ID)
	if err != nil || out.Outcome != "written" || out.Version != 1 {
		t.Fatalf("refresh = %+v %v", out, err)
	}
	got, _ = h.e.GetModel("b", m.ID)
	if got.Body != "Alice lives in Lyon." || got.Authority != "agent" || got.IsStale || len(got.BasedOn) == 0 {
		t.Fatalf("after refresh: %+v", got)
	}
	if out, _ := h.e.RefreshModel(context.Background(), "b", m.ID); out.Outcome != "unchanged" {
		t.Errorf("same answer = %s", out.Outcome)
	}

	// A person rewrites the answer in their editor.
	h.editFile(t, m.Path, "Alice lives in Lyon.", "Alice lives in Lyon, near her sister (my note).")
	got, _ = h.e.GetModel("b", m.ID)
	if got.Authority != "human" {
		t.Fatalf("edited body must read as human: %+v", got)
	}
	answer = "Alice has lived in Lyon since 2023."
	out, err = h.e.RefreshModel(context.Background(), "b", m.ID)
	if err != nil || out.Outcome != "proposed" {
		t.Fatalf("refresh over an edit = %+v %v", out, err)
	}
	if !strings.Contains(h.read(t, m.Path), "near her sister (my note)") {
		t.Fatal("the person's text was overwritten")
	}
	got, _ = h.e.GetModel("b", m.ID)
	if got.Proposal == nil || got.Proposal.Body != answer {
		t.Fatalf("proposal = %+v", got.Proposal)
	}
	// A rebuild still knows whose text it is, and still has the proposal.
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	got, _ = h.e.GetModel("b", m.ID)
	if got.Authority != "human" || got.Proposal == nil {
		t.Fatalf("after reindex: %+v", got)
	}
	// Rejecting keeps the person's text; a new refresh proposes again.
	if err := h.e.RejectProposal("b", m.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ = h.e.GetModel("b", m.ID); got.Proposal != nil || !strings.Contains(got.Body, "my note") {
		t.Fatalf("reject: %+v", got)
	}
	h.e.RefreshModel(context.Background(), "b", m.ID)
	acc, err := h.e.AcceptProposal("b", m.ID)
	if err != nil || acc.Body != answer || acc.Authority != "agent" || acc.Version != 2 {
		t.Fatalf("accept = %+v %v", acc, err)
	}
	if hist.count(m.Path) < 1 {
		t.Errorf("versions must go to history: %v", hist.rels)
	}
	// New memories in scope make the model stale.
	h.retain(t, "b", Item{Content: "Alice: my sister Emma visited.", DocumentID: "d2"})
	if got, _ = h.e.GetModel("b", m.ID); !got.IsStale || got.StaleReason != "memories_changed" {
		t.Errorf("stale = %+v", got)
	}
	ids, _ := h.e.staleAutoModels("b")
	if len(ids) != 1 {
		t.Errorf("auto models to refresh = %v", ids)
	}
}

func TestModelTreeExportAndMove(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "Alice lives in Lyon.", DocumentID: "d1"})
	for _, s := range []ModelSpec{
		{ID: "people/alice", Name: strPtr("Alice"), Question: strPtr("Who is Alice?"), Body: strPtr("A person.")},
		{ID: "people/team/bob", Name: strPtr("Bob"), Question: strPtr("Who is Bob?")},
		{ID: "overview", Name: strPtr("Overview"), Question: strPtr("What is this bank about?")},
	} {
		if _, err := h.e.CreateModel("b", s); err != nil {
			t.Fatal(err)
		}
	}
	if m, err := h.e.CreateModel("b", ModelSpec{ID: "carol", Folder: strPtr("people"), Question: strPtr("Who is Carol?")}); err != nil || m.ID != "people/carol" {
		t.Errorf("id with a folder = %+v %v", m, err)
	} else if err := h.e.DeleteModel("b", m.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.CreateModel("b", ModelSpec{ID: "people/alice", Question: strPtr("again")}); err != ErrExists {
		t.Errorf("duplicate id = %v", err)
	}
	tree, err := h.e.ModelTree("b", ModelQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(tree) != 2 || tree[0].Kind != "folder" || tree[0].Name != "people" || len(tree[0].Children) != 2 ||
		tree[0].Children[0].Kind != "folder" || tree[1].Path != "overview" {
		t.Fatalf("tree = %+v", tree)
	}
	files, _ := h.e.ExportModels("b")
	if len(files) != 4 || !strings.Contains(files[0].Content, "[Alice](./people/alice.md)") ||
		!strings.Contains(files[1].Content+files[2].Content+files[3].Content, "A person.") {
		t.Errorf("export = %+v", files)
	}
	moved, err := h.e.UpdateModel("b", "people/team/bob", ModelSpec{Folder: strPtr("")})
	if err != nil || moved.ID != "bob" {
		t.Fatalf("move = %+v %v", moved, err)
	}
	if _, err := h.e.GetModel("b", "people/team/bob"); err != ErrNotFound {
		t.Errorf("old id still resolves: %v", err)
	}
	// No model: refresh says so rather than writing an extractive answer.
	if _, err := h.e.RefreshModel(context.Background(), "b", "overview"); err != ErrModelRequired {
		t.Errorf("refresh with no model = %v", err)
	}
	if err := h.e.DeleteModel("b", "overview"); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.e.ListModels("b", ModelQuery{}); len(list) != 2 {
		t.Errorf("after delete: %d", len(list))
	}
}
