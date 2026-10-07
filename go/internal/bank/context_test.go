package bank

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFitContextDropsLowestValueFirstAndStaysUnderLimit(t *testing.T) {
	var items []ContextItem
	items = append(items, ContextItem{Kind: KindDirective, Text: "- always run the linter"})
	for i := 0; i < 60; i++ {
		items = append(items, ContextItem{Kind: KindFact, Text: fmt.Sprintf("- fact number %d about caching %s", i, strings.Repeat("x", 40))})
	}
	for _, limit := range []int{400, 900, 2000, 9000} {
		out, kept, dropped := FitContext("b", items, limit)
		if n := utf8.RuneCountInString(out); n > limit {
			t.Errorf("limit %d: rendered %d chars", limit, n)
		}
		if kept+dropped != len(items) || kept < 1 {
			t.Errorf("limit %d: kept %d dropped %d", limit, kept, dropped)
		}
		if !strings.Contains(out, "always run the linter") {
			t.Errorf("limit %d: the most valuable item was dropped", limit)
		}
		// What survives is a prefix: fact 0 before fact 1, never fact 5 without fact 4.
		for i := 1; i < 60; i++ {
			if strings.Contains(out, fmt.Sprintf("fact number %d ", i)) && !strings.Contains(out, fmt.Sprintf("fact number %d ", i-1)) {
				t.Errorf("limit %d: fact %d kept after dropping fact %d", limit, i, i-1)
			}
		}
		if limit == 400 && dropped == 0 {
			t.Error("nothing was dropped at a tight limit")
		}
	}
}

func TestFitContextTrimsASingleOversizedItem(t *testing.T) {
	big := ContextItem{Kind: KindModel, Text: "### Architecture\n" + strings.Repeat("a long line of prose\n", 200)}
	out, kept, _ := FitContext("b", []ContextItem{big}, 500)
	if kept != 1 || utf8.RuneCountInString(out) > 500 || !strings.Contains(out, "Architecture") {
		t.Fatalf("kept=%d len=%d", kept, utf8.RuneCountInString(out))
	}
}

func TestSessionContextUsesNoModelAndRespectsTheLimit(t *testing.T) {
	h := newHarness(t, false)
	if _, err := h.e.CreateDirective("b", DirectiveSpec{Name: ptr("lint"), Text: ptr("Run the linter before committing")}); err != nil {
		// A missing bank is created by the first retain; retry after one.
		h.retain(t, "b", Item{Content: "Alice uses Postgres. The cache is keyed by commit.", DocumentID: "d"})
		if _, err := h.e.CreateDirective("b", DirectiveSpec{Name: ptr("lint"), Text: ptr("Run the linter before committing")}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 80; i++ {
		h.retain(t, "b", Item{Content: fmt.Sprintf("Fact %d: the service %d listens on port %d.", i, i, 8000+i), DocumentID: fmt.Sprintf("x%d", i)})
	}
	small, err := h.e.SessionContext("b", ContextOptions{MaxChars: 800})
	if err != nil {
		t.Fatal(err)
	}
	if small.Chars > 800 || small.Dropped == 0 || !strings.Contains(small.Context, "Run the linter") {
		t.Fatalf("small = %d chars, dropped %d\n%s", small.Chars, small.Dropped, small.Context)
	}
	def, _ := h.e.SessionContext("b", ContextOptions{})
	if def.Limit != DefaultContextChars || def.Chars > DefaultContextChars {
		t.Fatalf("default limit = %d, chars %d", def.Limit, def.Chars)
	}
	if _, err := h.e.SessionContext("nope", ContextOptions{}); err == nil {
		t.Fatal("an unknown bank must not render")
	}
}

func ptr[T any](v T) *T { return &v }
