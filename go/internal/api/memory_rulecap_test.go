package api

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

// ruleItems are five rule candidates from notes, each with its own text.
func ruleItems(n int) []contextItem {
	var items []contextItem
	for i := 0; i < n; i++ {
		items = append(items, contextItem{Kind: "rule", Path: fmt.Sprintf("Agent Memory/rule-%d.md", i),
			Text: fmt.Sprintf("Rule number %d: always check the thing before the other thing.", i)})
	}
	return items
}

// writeDirective runs writeContext in directive form and decodes the response.
func writeDirective(t *testing.T, s *Server, items []contextItem) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	s.writeContext(rec, items, map[string]bool{}, 8000, 20, 0, "lexical", true, ctxLog{})
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRuleCapFlagOffUnchanged(t *testing.T) {
	restore := exp.Set()
	defer restore()
	srv, _ := testServer(t)
	items := append(ruleItems(5), contextItem{Kind: "fact", Text: "The kestrel lives on the roof."})
	out := writeDirective(t, srv, items)
	if n := len(out["keys"].([]any)); n != 6 {
		t.Fatalf("flag off should keep all 6 items, kept %d", n)
	}
	if _, ok := out["rules_capped"]; ok {
		t.Fatalf("flag off must not report rules_capped: %v", out)
	}
	if strings.Contains(out["context"].(string), "applied in") {
		t.Fatalf("flag off must not render precision: %v", out["context"])
	}
}

func TestRuleCapLimitsRulesAndKeepsOtherItems(t *testing.T) {
	restore := exp.Set("rulecap")
	defer restore()
	srv, _ := testServer(t)
	items := append(ruleItems(5), contextItem{Kind: "fact", Text: "The kestrel lives on the roof."})
	out := writeDirective(t, srv, items)
	if n := len(out["keys"].([]any)); n != 4 {
		t.Fatalf("cap of 3 rules plus one fact should keep 4 items, kept %d", n)
	}
	if got := out["rules_capped"]; got != float64(2) {
		t.Fatalf("rules_capped = %v, want 2", got)
	}
	if !strings.Contains(out["context"].(string), "The kestrel lives on the roof.") {
		t.Fatalf("fact dropped by the rule cap: %v", out["context"])
	}
}

func TestRuleCapSkippedRulesDoNotCountAgainstLimit(t *testing.T) {
	restore := exp.Set("rulecap")
	defer restore()
	srv, _ := testServer(t)
	// Rule candidates ranked first; the fourth candidate is a fact that must
	// still be picked even though three rules were already taken.
	items := append(ruleItems(5), contextItem{Kind: "fact", Text: "Fact after the rules."})
	rec := httptest.NewRecorder()
	srv.writeContext(rec, items, map[string]bool{}, 8000, 4, 0, "lexical", true, ctxLog{})
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if n := len(out["keys"].([]any)); n != 4 {
		t.Fatalf("limit 4 with 3 rules and a fact should pick 4, picked %d", n)
	}
}

func TestRulePrecisionSuffixNeedsFiveLabels(t *testing.T) {
	restore := exp.Set("rulecap")
	defer restore()
	srv, _ := testServer(t)
	st, _ := srv.rules()
	pol := rulecheck.DefaultPolicy()
	enough := "note:Agent Memory/rule-0.md"
	short := "note:Agent Memory/rule-1.md"
	for _, row := range []rulecheck.Row{
		{ID: "aaaa0001", Target: enough, RuleText: "rule", Spec: rulecheck.Spec{Shape: rulecheck.ShapeRequireBefore}, Labelled: 5, True: 4},
		{ID: "aaaa0002", Target: short, RuleText: "rule", Spec: rulecheck.Spec{Shape: rulecheck.ShapeRequireBefore}, Labelled: 4, True: 4},
	} {
		if err := st.ReplaceTarget(row.Target, []rulecheck.Row{row}, pol); err != nil {
			t.Fatal(err)
		}
	}
	out := writeDirective(t, srv, ruleItems(2))
	ctx := out["context"].(string)
	if !strings.Contains(ctx, "rule-0.md; applied in 4 of 5 checked firings]") {
		t.Fatalf("5 labels should render the suffix:\n%s", ctx)
	}
	if strings.Contains(ctx, "rule-1.md; applied") {
		t.Fatalf("4 labels must not render the suffix:\n%s", ctx)
	}
}

func TestRuleAppliedPicksMostLabelledCheck(t *testing.T) {
	rows := []rulecheck.Row{
		{Target: "note:Agent Memory/x.md", Labelled: 6, True: 6},
		{Target: "note:Agent Memory/x.md", Labelled: 9, True: 3},
		{Target: "note:Agent Memory/other.md", Labelled: 50, True: 1},
	}
	if got := ruleApplied(rows, "Agent Memory/x.md"); got != "; applied in 3 of 9 checked firings" {
		t.Fatalf("got %q", got)
	}
	if got := ruleApplied(rows, "Agent Memory/none.md"); got != "" {
		t.Fatalf("no check should render nothing, got %q", got)
	}
}
