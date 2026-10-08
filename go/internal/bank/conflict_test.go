package bank

import (
	"strings"
	"testing"
)

func TestRuleConflictShapes(t *testing.T) {
	ents := []string{"Alice"}
	for _, c := range []struct {
		h, m string
		want bool
	}{
		{"Alice lives in Lyon", "Alice lives in Paris", true},
		{"Alice lives in Lyon", "Alice lives in Lyon", false},
		{"Alice likes tea", "Alice does not like tea", true},
		{"Alice plays chess on Sundays", "Alice lives in Paris", false},
		{"Alice works at Shopify", "Alice works at Shopify Canada", false},
		{"Alice works at Shopify", "Alice works at Stripe", true},
	} {
		if got := ruleConflict(c.h, ents, c.m, ents); got != c.want {
			t.Errorf("%q vs %q = %v, want %v", c.h, c.m, got, c.want)
		}
	}
	if ruleConflict("Alice lives in Lyon", []string{"Alice"}, "Bob lives in Paris", []string{"Bob"}) {
		t.Error("different entities never conflict")
	}
}

func conflictHarness(t *testing.T, judge func(user string) string) (*harness, string) {
	h := newHarness(t, true)
	city := "Lyon"
	h.llm.route = func(sys, user string) (string, string) {
		if strings.Contains(sys, "cannot both be true") {
			return judge(user), "stop"
		}
		return livesIn(city)(user)
	}
	h.retain(t, "b", Item{Content: "Alice: I moved to Lyon last year.", DocumentID: "a"})
	h.editFile(t, FactsPath("b", "a"), "Alice lives in Lyon <!--f", "Alice lives in Lyon, France <!--f")
	u := unitByText(t, h, "b", "Alice lives in Lyon, France")
	if u == nil || !u.Human {
		t.Fatalf("setup: %+v", u)
	}
	city = "Paris"
	h.retain(t, "b", Item{Content: "Notes from a call: Alice mentioned her address in Paris.", DocumentID: "b2"})
	return h, u.ID
}

func checkDisputed(t *testing.T, h *harness, humanID string) {
	t.Helper()
	file := h.read(t, FactsPath("b", "b2"))
	if !strings.Contains(file, "chal="+humanID) {
		t.Fatalf("the model fact must carry the challenge:\n%s", file)
	}
	if !strings.Contains(h.read(t, FactsPath("b", "a")), "Alice lives in Lyon, France <!--f id="+humanID) {
		t.Fatalf("the person's fact changed:\n%s", h.read(t, FactsPath("b", "a")))
	}
	r := h.recall(t, "b", "Where does Alice live?")
	var human, paris = -1, -1
	for i, f := range r.Results {
		switch f.Text {
		case "Alice lives in Lyon, France":
			human = i
		case "Alice lives in Paris":
			paris = i
			if f.DisputedBy != humanID {
				t.Errorf("Paris must be disputed by the person's fact: %+v", f)
			}
		}
	}
	if human < 0 || (paris >= 0 && paris < human) {
		t.Fatalf("the person's fact must rank first: human=%d paris=%d", human, paris)
	}
}

func TestCrossDocumentConflictRulesFallback(t *testing.T) {
	h, id := conflictHarness(t, func(string) string { return "not json" })
	checkDisputed(t, h, id)
	// Survives a rebuild purely from the files.
	if _, err := h.ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	checkDisputed(t, h, id)
}

func TestCrossDocumentConflictUsesTheModelWhenConfigured(t *testing.T) {
	// The model says nothing contradicts: no challenge is written by retain
	// (recall's rule still shows the dispute, which is read-only).
	h, id := conflictHarness(t, func(string) string { return `{"contradict":[]}` })
	if strings.Contains(h.read(t, FactsPath("b", "b2")), "chal=") {
		t.Fatal("the model cleared the pair, so retain must not record a challenge")
	}
	_ = id
	h, id = conflictHarness(t, func(string) string { return `{"contradict":[0,1]}` })
	checkDisputed(t, h, id)
}
