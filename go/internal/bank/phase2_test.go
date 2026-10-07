package bank

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// reverser scores later candidates higher, so a reranked order is visibly
// not the fused one.
type reverser struct{ seen [][]string }

func (r *reverser) Name() string { return "reverser" }
func (r *reverser) Score(_ context.Context, _ string, docs []string) ([]float32, error) {
	r.seen = append(r.seen, docs)
	out := make([]float32, len(docs))
	for i := range docs {
		out[i] = float32(i) / float32(len(docs))
	}
	return out, nil
}

type failing struct{}

func (failing) Score(context.Context, string, []string) ([]float32, error) {
	return nil, errors.New("model file missing")
}

func TestRecallUsesTheRerankerAndSurvivesItFailing(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b", Item{Content: "Alice adopted a beagle on 2023-05-02. Alice walks the beagle daily. Bob has a cat.",
		DocumentID: "d1", Context: "pet chat"})
	rr := &reverser{}
	h.e.Reranker = rr
	r := h.recall(t, "b", "Alice beagle")
	if !r.Trace.Reranked || r.Trace.Reranker != "reverser" || len(r.Trace.Rerank) == 0 {
		t.Fatalf("trace = %+v", r.Trace)
	}
	if r.Results[0].Scores.Reranker == nil {
		t.Error("results must carry the reranker score")
	}
	doc := rr.seen[0][0]
	if !strings.Contains(doc, "pet chat: ") {
		t.Errorf("rerank text lacks context: %q", doc)
	}
	dated := false
	for _, d := range rr.seen[0] {
		dated = dated || strings.HasPrefix(d, "[Date: May 02, 2023 (2023-05-02)] pet chat: ")
	}
	if !dated {
		t.Errorf("rerank text lacks the date prefix: %q", rr.seen[0])
	}
	// The bank can turn it off.
	h.e.UpdateProfile("b", func(p *Profile) error { p.Config["enable_reranking"] = "false"; return nil })
	if r := h.recall(t, "b", "Alice beagle"); r.Trace.Reranked {
		t.Error("enable_reranking=false must skip the reranker")
	}
	h.e.UpdateProfile("b", func(p *Profile) error { delete(p.Config, "enable_reranking"); return nil })
	h.e.Reranker = failing{}
	r = h.recall(t, "b", "Alice beagle")
	if r.Trace.Reranked || r.Trace.RerankError == "" || len(r.Results) == 0 {
		t.Errorf("a failing reranker must cost the order, not the answer: %+v", r.Trace)
	}
}

func TestTagGroupsExactWindowAndMinScores(t *testing.T) {
	h := newHarness(t, false)
	h.retain(t, "b",
		Item{Content: "Alpha project launched in March.", DocumentID: "a", Tags: []string{"team:a", "q1"}, Timestamp: ts("2024-03-10T00:00:00Z")},
		Item{Content: "Alpha project hired two people.", DocumentID: "b", Tags: []string{"team:a"}, Timestamp: ts("2024-06-10T00:00:00Z")},
		Item{Content: "Alpha project review went well.", DocumentID: "c", Tags: []string{"team:b"}, Timestamp: ts("2024-09-10T00:00:00Z")},
		Item{Content: "Alpha project untagged note.", DocumentID: "d", Timestamp: ts("2024-12-10T00:00:00Z")},
	)
	docs := func(r *RecallResponse) string {
		var out []string
		for _, f := range r.Results {
			out = append(out, f.DocumentID)
		}
		return strings.Join(out, ",")
	}
	r := h.recall(t, "b", "alpha project", func(q *RecallRequest) {
		q.TagGroups = []TagGroup{{Tags: []string{"team:a"}}, {Not: &TagGroup{Tags: []string{"q1"}}}}
	})
	if docs(r) != "b" {
		t.Errorf("team:a AND NOT q1 = %s", docs(r))
	}
	r = h.recall(t, "b", "alpha project", func(q *RecallRequest) {
		q.TagGroups = []TagGroup{{Or: []TagGroup{{Tags: []string{"team:b"}}, {Tags: []string{"q1"}}}}}
	})
	if got := docs(r); !strings.Contains(got, "a") || !strings.Contains(got, "c") || strings.Contains(got, "d") {
		t.Errorf("team:b OR q1 = %s", got)
	}
	r = h.recall(t, "b", "alpha project", func(q *RecallRequest) { q.Tags, q.TagsMatch = []string{"team:a"}, "exact" })
	if docs(r) != "b" {
		t.Errorf("exact team:a = %s", docs(r))
	}
	r = h.recall(t, "b", "alpha project", func(q *RecallRequest) { q.TagsMatch = "exact" })
	if docs(r) != "d" {
		t.Errorf("exact with no tags selects the untagged: %s", docs(r))
	}
	if _, err := h.e.Recall(context.Background(), "b", RecallRequest{Query: "x", TagGroups: []TagGroup{{Tags: []string{"a"}, And: []TagGroup{{}}}}}); err == nil {
		t.Error("a group with two shapes must be refused")
	}
	w := Window{Start: time.Date(2024, 9, 1, 0, 0, 0, 0, time.UTC), End: time.Date(2024, 9, 30, 0, 0, 0, 0, time.UTC)}
	r = h.recall(t, "b", "alpha project", func(q *RecallRequest) { q.Window = &w })
	// The window ranks rather than filters: its inside fact leads the
	// temporal arm.
	if r.Trace.Window == nil || !r.Trace.Window.Start.Equal(w.Start) || len(r.Trace.Arms["temporal"]) == 0 {
		t.Fatalf("temporal_window: %+v", r.Trace)
	}
	if f, _ := h.e.GetFact("b", r.Trace.Arms["temporal"][0].ID); f == nil || f.DocumentID != "c" {
		t.Errorf("temporal arm led by %+v", f)
	}
	high := 2.0
	r = h.recall(t, "b", "alpha project", func(q *RecallRequest) { q.MinScores = &MinScores{Final: &high} })
	if len(r.Results) != 0 {
		t.Errorf("min final score 2 keeps nothing: %s", docs(r))
	}
}

func TestDirectivesCRUDEditsBankMd(t *testing.T) {
	h := newHarness(t, false)
	h.e.CreateBank(NewProfile("b"))
	d, err := h.e.CreateDirective("b", DirectiveSpec{Name: strPtr("Lang"), Text: strPtr("Answer in French."), Tags: &[]string{"fr"}, Priority: intPtr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.e.CreateDirective("b", DirectiveSpec{Name: strPtr("lang"), Text: strPtr("dup")}); err == nil {
		t.Error("duplicate names must be refused")
	}
	file := h.read(t, ProfilePath("b"))
	if !strings.Contains(file, "- Answer in French. <!--d id="+d.ID+" name=Lang prio=2 tags=fr -->") {
		t.Fatalf("bank.md:\n%s", file)
	}
	off := false
	if _, err := h.e.UpdateDirective("b", d.ID, DirectiveSpec{Active: &off}); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.e.ListDirectives("b", nil, true); len(list) != 0 {
		t.Errorf("inactive directive listed as active: %+v", list)
	}
	if err := h.e.DeleteDirective("b", d.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.e.DeleteDirective("b", d.ID); err != ErrNotFound {
		t.Errorf("delete twice = %v", err)
	}
}

func intPtr(n int) *int { return &n }

func TestTemplatesImportAdditivelyAndExport(t *testing.T) {
	h := newHarness(t, false)
	tpl, ok := BuiltinTemplate("coding-agent")
	if !ok {
		t.Fatal("coding-agent template missing")
	}
	dry, err := h.e.ImportTemplate("coding-agent:grimoire", tpl.Manifest, true)
	if err != nil || !dry.BankCreated || len(dry.MentalModelsCreated) != 3 || !dry.DryRun {
		t.Fatalf("dry run = %+v %v", dry, err)
	}
	if _, err := h.e.Profile("coding-agent:grimoire"); err != ErrNotFound {
		t.Fatal("a dry run must write nothing")
	}
	res, err := h.e.ImportTemplate("coding-agent:grimoire", tpl.Manifest, false)
	if err != nil || len(res.MentalModelsCreated) != 3 || len(res.DirectivesCreated) != 1 {
		t.Fatalf("import = %+v %v", res, err)
	}
	p, _ := h.e.Profile("coding-agent:grimoire")
	if p.Disposition.Literalism != 5 || p.Config["consolidation"] != "auto" || !strings.Contains(p.RetainMission, "technical decisions") {
		t.Errorf("profile = %+v", p)
	}
	// A person's own setup survives a re-import: their setting, directive and
	// answer stay; only what the manifest names is set.
	h.e.UpdateProfile("coding-agent:grimoire", func(p *Profile) error { p.Config["retain_chunk_size"] = "2000"; return nil })
	h.e.CreateDirective("coding-agent:grimoire", DirectiveSpec{Text: strPtr("Mine.")})
	h.e.UpdateModel("coding-agent:grimoire", "project-context", ModelSpec{Body: strPtr("My own summary.")})
	res, err = h.e.ImportTemplate("coding-agent:grimoire", tpl.Manifest, false)
	if err != nil || len(res.MentalModelsUpdated) != 3 || len(res.DirectivesUpdated) != 1 || len(res.DirectivesCreated) != 0 {
		t.Fatalf("re-import = %+v %v", res, err)
	}
	p, _ = h.e.Profile("coding-agent:grimoire")
	if p.Config["retain_chunk_size"] != "2000" || len(p.Directives) != 2 {
		t.Errorf("re-import removed something: %+v", p)
	}
	if m, _ := h.e.GetModel("coding-agent:grimoire", "project-context"); m.Body != "My own summary." {
		t.Errorf("re-import touched an answer: %q", m.Body)
	}
	man, err := h.e.ExportTemplate("coding-agent:grimoire")
	if err != nil || len(man.MentalModels) != 3 || len(man.Directives) != 2 || man.Bank.Config["retain_chunk_size"] != "2000" {
		t.Fatalf("export = %+v %v", man, err)
	}
	// What was exported imports cleanly elsewhere.
	if _, err := h.e.ImportTemplate("copy", *man, false); err != nil {
		t.Errorf("re-importing an export: %v", err)
	}
	bad := Manifest{Version: "9"}
	if _, err := h.e.ImportTemplate("x", bad, true); err == nil {
		t.Error("unknown manifest version accepted")
	}
}

func TestDeleteBankClearsItsOperationalState(t *testing.T) {
	h := newHarness(t, false)
	h.e.AllowPrivateWebhooks = func() bool { return true }
	h.retain(t, "b", Item{Content: "x.", DocumentID: "d"})
	h.e.CreateWebhook(context.Background(), "b", WebhookSpec{URL: strPtr("http://127.0.0.1:9/")})
	id, _ := h.e.EnqueueRetain("b", []Item{{Content: "y"}}, RetainOptions{})
	if err := h.e.DeleteBank("b"); err != nil {
		t.Fatal(err)
	}
	op, _ := h.e.GetOperation("b", id)
	if op.Status != OpCancelled {
		t.Errorf("queued work on a deleted bank = %s", op.Status)
	}
	if hooks, _ := h.e.ListWebhooks("b"); len(hooks) != 0 {
		t.Errorf("webhooks kept: %+v", hooks)
	}
}
