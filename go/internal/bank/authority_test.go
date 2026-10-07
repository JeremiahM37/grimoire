package bank

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// livesIn scripts a model that reads "Alice lives in <city>" off any content
// mentioning Alice, plus one unrelated fact.
func livesIn(city string) func(string) (string, string) {
	return func(user string) (string, string) {
		return extractionReply(
			map[string]any{"what": "Alice lives in " + city, "fact_type": "world", "fact_kind": "conversation",
				"entities": []any{"Alice", city}},
			map[string]any{"what": "Alice plays chess on Sundays", "fact_type": "world", "fact_kind": "conversation",
				"entities": []any{"Alice", "chess"}},
		), "stop"
	}
}

func unitByText(t *testing.T, h *harness, bank, text string) *unit {
	t.Helper()
	c, err := h.e.cache(bank)
	if err != nil {
		t.Fatal(err)
	}
	for i := range c.units {
		if c.units[i].Text == text {
			return &c.units[i]
		}
	}
	return nil
}

func TestPersonsEditIsHumanAndSurvivesReRetain(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year and I play chess.", DocumentID: "conv"})
	rel := FactsPath("b", "conv")

	// A person corrects the fact in their editor: text changed, trailer not.
	h.editFile(t, rel, "Alice lives in Lyon <!--f", "Alice lives in Paris <!--f")
	u := unitByText(t, h, "b", "Alice lives in Paris")
	if u == nil || !u.Human {
		t.Fatalf("an edited fact must read as human: %+v", u)
	}
	humanID := u.ID

	// Re-retaining identical content changes nothing, and keeps the edit.
	before := h.llm.calls.Load()
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year and I play chess.", DocumentID: "conv"})
	if h.llm.calls.Load() != before {
		t.Error("identical content must not be re-extracted")
	}
	file := h.read(t, rel)
	if !strings.Contains(file, "Alice lives in Paris <!--f id="+humanID+" by=human") || strings.Contains(file, "doc_removed") {
		t.Fatalf("edit lost or wrongly marked on an unchanged re-retain:\n%s", file)
	}

	// The source changes, so its chunk is re-extracted — and the model says
	// Lyon again. The person's Paris stays, marked as no longer backed by the
	// source text, and the model's Lyon comes back beside it.
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year and I play chess every Sunday.", DocumentID: "conv"})
	file = h.read(t, rel)
	if !strings.Contains(file, "Alice lives in Paris <!--f id="+humanID+" by=human") {
		t.Fatalf("re-retain deleted or rewrote the person's fact:\n%s", file)
	}
	if !strings.Contains(file, "doc_removed") || !strings.Contains(file, "Alice lives in Lyon <!--f") {
		t.Errorf("expected the human fact marked doc_removed and the model fact restored:\n%s", file)
	}

	// Recall: the conflicting extracted fact loses.
	r := h.recall(t, "b", "Alice lives in Lyon")
	if len(r.Results) < 2 {
		t.Fatalf("results = %v", texts2(r.Results))
	}
	if r.Results[0].ID != humanID || r.Results[0].Authority != "human" {
		t.Errorf("the human fact must rank first even on a query worded like the model's: %v", texts2(r.Results))
	}
	var lyon *RecallFact
	for i := range r.Results {
		if r.Results[i].Text == "Alice lives in Lyon" {
			lyon = &r.Results[i]
		}
	}
	if lyon == nil || lyon.DisputedBy != humanID {
		t.Errorf("the model fact must be marked disputed by the human one: %+v", lyon)
	}
}

func TestDocumentReplaceAndDeleteKeepHumanFacts(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	h.retain(t, "b", Item{Content: "Alice: I live in Lyon.", DocumentID: "d"})
	rel := FactsPath("b", "d")
	h.editFile(t, rel, "Alice plays chess on Sundays <!--f", "Alice plays chess on Saturdays <!--f")

	// Replace the document with unrelated content.
	h.llm.reply = func(string) (string, string) {
		return extractionReply(map[string]any{"what": "The office moved to Berlin", "fact_type": "world",
			"entities": []any{"office", "Berlin"}}), "stop"
	}
	h.retain(t, "b", Item{Content: "Memo: the office moved to Berlin.", DocumentID: "d"})
	file := h.read(t, rel)
	if !strings.Contains(file, "Alice plays chess on Saturdays") || strings.Contains(file, "Alice lives in Lyon") {
		t.Fatalf("replace must drop the model's facts and keep the person's:\n%s", file)
	}
	if !strings.Contains(file, "by=human") || !strings.Contains(file, "doc_removed") {
		t.Errorf("kept fact must be declared human and marked doc_removed:\n%s", file)
	}

	// Deleting the document keeps it too, unless forced.
	kept, err := h.e.DeleteDocument("b", "d", false)
	if err != nil || kept != 1 {
		t.Fatalf("delete kept %d: %v", kept, err)
	}
	if u := unitByText(t, h, "b", "Alice plays chess on Saturdays"); u == nil || !u.Human || !u.DocRemoved {
		t.Errorf("after delete: %+v", u)
	}
	if u := unitByText(t, h, "b", "The office moved to Berlin"); u != nil {
		t.Error("the model's fact must go with its document")
	}
	// Deleting the human fact itself needs force.
	u := unitByText(t, h, "b", "Alice plays chess on Saturdays")
	if err := h.e.DeleteFact("b", u.ID, false); !errors.Is(err, ErrHumanProtected) {
		t.Errorf("unforced delete of a human fact = %v", err)
	}
	if err := h.e.DeleteFact("b", u.ID, true); err != nil {
		t.Errorf("forced delete = %v", err)
	}
}

func TestHandTypedBulletIsAHumanFact(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	h.retain(t, "b", Item{Content: "Alice: I live in Lyon.", DocumentID: "d"})
	rel := FactsPath("b", "d")
	h.editFile(t, rel, "# Facts: d\n", "# Facts: d\n\n- Alice is allergic to peanuts\n\nNotes a person keeps here.\n")
	u := unitByText(t, h, "b", "Alice is allergic to peanuts")
	if u == nil || !u.Human || !strings.HasPrefix(u.ID, "h") {
		t.Fatalf("a bullet with no trailer is a person's: %+v", u)
	}
	h.retain(t, "b", Item{Content: "Alice: I live in Lyon, near the river.", DocumentID: "d"})
	file := h.read(t, rel)
	if !strings.Contains(file, "Alice is allergic to peanuts <!--f id="+u.ID+" by=human") {
		t.Errorf("hand-typed fact lost on re-retain:\n%s", file)
	}
	if !strings.Contains(file, "Notes a person keeps here.") {
		t.Errorf("prose between bullets lost:\n%s", file)
	}
	if strings.Contains(file, "allergic to peanuts <!--f id="+u.ID+" by=human sum") && strings.Contains(
		file[strings.Index(file, "allergic"):], "doc_removed") && strings.Index(file[strings.Index(file, "allergic"):], "doc_removed") < 120 {
		t.Error("a fact typed by hand belongs to no chunk, so the source changing does not orphan it")
	}
}

func TestReindexRebuildsBanksFromFilesWithoutTheModel(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	h.retain(t, "b", Item{Content: "Alice: I live in Lyon and play chess.", DocumentID: "d1", Tags: []string{"t1"}})
	h.retain(t, "b", Item{Content: "Alice: chess on Sundays, still in Lyon.", DocumentID: "d2"})
	h.editFile(t, FactsPath("b", "d1"), "Alice lives in Lyon <!--f", "Alice lives in Paris <!--f")
	before := h.recall(t, "b", "where does Alice live")
	c0, _ := h.e.cache("b")
	n0, ents0 := len(c0.units), len(c0.entities)
	calls := h.llm.calls.Load()

	// Wipe every cache row the hard way, then rebuild from the vault.
	for _, tbl := range []string{"bank_units", "bank_units_fts", "bank_entities", "bank_unit_entities", "bank_links", "bank_documents", "bank_chunks", "bank_banks", "bank_vec_cache"} {
		if err := h.ix.DB.Exec("DELETE FROM " + tbl); err != nil {
			t.Fatal(err)
		}
	}
	h.e.bumpAll()
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	if h.llm.calls.Load() != calls {
		t.Fatalf("reindex called the model %d times", h.llm.calls.Load()-calls)
	}
	c1, _ := h.e.cache("b")
	if len(c1.units) != n0 || len(c1.entities) != ents0 {
		t.Fatalf("rebuilt %d facts / %d entities, had %d / %d", len(c1.units), len(c1.entities), n0, ents0)
	}
	u := unitByText(t, h, "b", "Alice lives in Paris")
	if u == nil || !u.Human {
		t.Fatalf("authorship must survive a rebuild from the files: %+v", u)
	}
	after := h.recall(t, "b", "where does Alice live")
	if len(after.Results) != len(before.Results) || after.Results[0].ID != before.Results[0].ID {
		t.Errorf("recall changed across a rebuild:\nbefore %v\nafter  %v", texts2(before.Results), texts2(after.Results))
	}
	banks, err := h.e.ListBanks()
	if err != nil || len(banks) != 1 || banks[0].Facts != n0 || banks[0].Documents != 2 {
		t.Errorf("banks = %+v %v", banks, err)
	}
	ch, err := h.e.GetChunk("b", ChunkID("b", "d1", 0))
	if err != nil || !strings.Contains(ch.Text, "Lyon") {
		t.Errorf("chunk = %+v %v", ch, err)
	}
}

func TestUsageIsBookedAgainstTheRetainSurface(t *testing.T) {
	h := newHarness(t, true)
	h.llm.reply = livesIn("Lyon")
	res, err := h.e.Retain(context.Background(), "b", []Item{{Content: "Alice: hi from Lyon", DocumentID: "d"}}, RetainOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.Input != 100 || res.Usage.Output != 20 {
		t.Errorf("usage = %+v", res.Usage)
	}
}
