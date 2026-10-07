package bank

import (
	"strings"
	"testing"
)

func dupFacts(t *testing.T, h *harness) {
	t.Helper()
	h.llm.route = func(sys, user string) (string, string) {
		switch {
		case strings.Contains(user, "Doc one"):
			return extractionReply(
				map[string]any{"what": "Priya deploys the billing service with the blue green rollout script", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Priya"}},
				map[string]any{"what": "Priya prefers tea", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Priya"}},
			), "stop"
		case strings.Contains(user, "Doc two"):
			return extractionReply(
				map[string]any{"what": "Priya deploys the billing service using the blue green rollout script", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Priya"}},
				map[string]any{"what": "Priya prefers coffee", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Priya"}},
			), "stop"
		}
		return extractionReply(
			map[string]any{"what": "The office is open on Monday", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Office"}},
			map[string]any{"what": "The office is open on Tuesday", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Office"}},
			map[string]any{"what": "The team is open to ideas", "fact_type": "world", "fact_kind": "conversation", "entities": []any{"Team"}},
		), "stop"
	}
	h.retain(t, "b", Item{Content: "Doc one: deploys", DocumentID: "d1"}, Item{Content: "Doc two: deploys", DocumentID: "d2"},
		Item{Content: "Doc three: office", DocumentID: "d3"})
}

func TestDuplicateCandidatesUseIDFAndVetoCommonWords(t *testing.T) {
	h := newHarness(t, true)
	dupFacts(t, h)
	got, err := h.e.DuplicateCandidates("b", DuplicateQuery{MinScore: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Keep.Text, "blue green") || len(got[0].Shared) < 2 {
		t.Fatalf("candidates = %+v", got)
	}
	for _, c := range got {
		if strings.Contains(c.Keep.Text, "tea") || strings.Contains(c.Keep.Text, "office") || strings.Contains(c.Keep.Text, "open to") {
			t.Errorf("a pair sharing only common words must be vetoed: %+v", c)
		}
	}
}

func TestMergeDuplicatesStrikesThroughAndKeepsAPersonsText(t *testing.T) {
	h := newHarness(t, true)
	dupFacts(t, h)
	// A person owns the d2 version.
	h.editFile(t, FactsPath("b", "d2"), "using the blue green rollout script", "using the blue green rollout script (my wording)")
	got, _ := h.e.DuplicateCandidates("b", DuplicateQuery{MinScore: 0.4})
	if len(got) != 1 || !got[0].Keep.Human || got[0].Merge.Human {
		t.Fatalf("the person's version should be kept: %+v", got)
	}
	// Merging the person's into the model's is refused.
	if _, err := h.e.MergeDuplicates("b", got[0].Merge.ID, got[0].Keep.ID); err == nil {
		t.Fatal("a person's fact must not be merged into a model's")
	}
	res, err := h.e.MergeDuplicates("b", got[0].Keep.ID, got[0].Merge.ID)
	if err != nil || res.Type != "fact" {
		t.Fatalf("merge = %+v %v", res, err)
	}
	if !strings.Contains(h.read(t, FactsPath("b", "d2")), "(my wording)") {
		t.Fatal("the person's text must stay")
	}
	d1 := h.read(t, FactsPath("b", "d1"))
	if !strings.Contains(d1, "~~Priya deploys the billing service with the blue green rollout script~~ (merged into "+got[0].Keep.ID) {
		t.Fatalf("the merged-away text must be struck through and kept:\n%s", d1)
	}
	if again, _ := h.e.DuplicateCandidates("b", DuplicateQuery{MinScore: 0.4}); len(again) != 0 {
		t.Fatalf("still listed: %+v", again)
	}
	// Survives a rebuild and a re-retain of the document.
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.read(t, FactsPath("b", "d1")), "~~Priya deploys") {
		t.Fatal("lost after reindex")
	}
	h.retain(t, "b", Item{Content: "Doc one: deploys, and more words", DocumentID: "d1"})
	if !strings.Contains(h.read(t, FactsPath("b", "d1")), "~~Priya deploys") {
		t.Fatal("lost after the document was retained again")
	}
	// Merge a human fact into a human fact: both texts stay.
	h.editFile(t, FactsPath("b", "d1"), "Priya prefers tea", "Priya prefers tea (mine)")
	h.editFile(t, FactsPath("b", "d2"), "Priya prefers coffee", "Priya prefers tea or coffee (hers)")
	c, _ := h.e.DuplicateCandidates("b", DuplicateQuery{MinScore: 0.2})
	for _, p := range c {
		if p.Keep.Human && p.Merge.Human {
			if _, err := h.e.MergeDuplicates("b", p.Keep.ID, p.Merge.ID); err != nil {
				t.Fatal(err)
			}
			all := h.read(t, FactsPath("b", "d1")) + h.read(t, FactsPath("b", "d2"))
			if !strings.Contains(all, "(mine)") || !strings.Contains(all, "(hers)") {
				t.Fatalf("a human text vanished:\n%s", all)
			}
		}
	}
}

func TestMergeObservationsKeepsHistoryAndTheHumanOne(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.e.Profile("b"); err != nil {
		h.retain(t, "b", Item{Content: "seed.", DocumentID: "seed"})
	}
	of := ObservationsFile{Current: []Observation{
		{ID: "oa", Text: "Priya owns the billing service deployment pipeline", Sources: []string{"f1"}, Human: true},
		{ID: "ob", Text: "Priya owns the billing service deployment pipeline today", Sources: []string{"f2"}},
	}}
	if err := h.e.writeObservations("b", of, ""); err != nil {
		t.Fatal(err)
	}
	got, _ := h.e.DuplicateCandidates("b", DuplicateQuery{MinScore: 0.5, Types: "observation"})
	if len(got) != 1 || got[0].Keep.ID != "oa" {
		t.Fatalf("candidates = %+v", got)
	}
	if _, err := h.e.MergeDuplicates("b", "oa", "ob"); err != nil {
		t.Fatal(err)
	}
	file := h.read(t, ObservationsPath("b"))
	if !strings.Contains(file, "~~Priya owns the billing service deployment pipeline today~~") ||
		!strings.Contains(file, "Priya owns the billing service deployment pipeline <!--o id=oa by=human") ||
		!strings.Contains(file, "src=f1,f2") {
		t.Fatalf("observations file:\n%s", file)
	}
}
