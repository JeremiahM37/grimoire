package bank

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func seedDisclosure(t *testing.T, h *harness) {
	t.Helper()
	for i, text := range []string{
		"The cache is keyed by commit hash.",
		"Releases are tagged from the main branch.",
		"The linter runs before every commit.",
	} {
		h.retain(t, "b", Item{Content: text, DocumentID: fmt.Sprintf("d%d", i),
			Timestamp: ts(fmt.Sprintf("2026-10-0%dT10:00:00Z", i+1))})
	}
}

func TestIndexIsCompactCitableAndRanked(t *testing.T) {
	h := newHarness(t, false)
	seedDisclosure(t, h)
	items, total, err := h.e.BankIndex(context.Background(), "b", IndexQuery{})
	if err != nil || total != 3 || len(items) != 3 {
		t.Fatalf("items=%v total=%d err=%v", items, total, err)
	}
	if items[0].Date < items[1].Date || !strings.HasPrefix(items[0].Ref, "#") || len(items[0].Ref) != 1+refLen {
		t.Errorf("newest first with short refs: %+v", items)
	}
	ranked, _, _ := h.e.BankIndex(context.Background(), "b", IndexQuery{Query: "release tagged branch"})
	if len(ranked) == 0 || !strings.Contains(ranked[0].Title, "Releases") {
		t.Errorf("ranked = %+v", ranked)
	}
	page, total, _ := h.e.BankIndex(context.Background(), "b", IndexQuery{Limit: 2, Offset: 2})
	if len(page) != 1 || total != 3 {
		t.Errorf("page = %+v total %d", page, total)
	}
	since, _, _ := h.e.BankIndex(context.Background(), "b", IndexQuery{Since: "2026-10-02"})
	if len(since) != 2 {
		t.Errorf("since = %+v", since)
	}
	var tokens int
	for _, it := range items {
		tokens += len(it.Title)
	}
	if tokens > 3*titleChars {
		t.Error("titles are not bounded")
	}
}

func TestGetByIDsResolvesShortRefsAndReportsMisses(t *testing.T) {
	h := newHarness(t, false)
	seedDisclosure(t, h)
	items, _, _ := h.e.BankIndex(context.Background(), "b", IndexQuery{})
	got, missing, err := h.e.GetByIDs("b", []string{items[0].Ref, items[1].ID, items[0].Ref, "#nonexistent", "ab"})
	if err != nil || len(got) != 2 || len(missing) != 2 {
		t.Fatalf("got=%v missing=%v err=%v", got, missing, err)
	}
	if got[0].Text == "" || got[0].ID != items[0].ID {
		t.Errorf("got = %+v", got[0])
	}
}

func TestTimelineAroundAnEntryAndADay(t *testing.T) {
	h := newHarness(t, false)
	seedDisclosure(t, h)
	items, _, _ := h.e.BankIndex(context.Background(), "b", IndexQuery{})
	var middle IndexEntry
	for _, it := range items {
		if it.Date == "2026-10-02" {
			middle = it
		}
	}
	tl, err := h.e.Timeline("b", middle.Ref, 1, 1)
	if err != nil || len(tl.Entries) != 3 || tl.AnchorRef != middle.Ref {
		t.Fatalf("tl=%+v err=%v", tl, err)
	}
	if !(tl.Entries[0].Date <= tl.Entries[1].Date && tl.Entries[1].Date <= tl.Entries[2].Date) {
		t.Errorf("not oldest first: %+v", tl.Entries)
	}
	day, err := h.e.Timeline("b", "2026-10-03", 1, 0)
	if err != nil || len(day.Entries) != 1 || day.Entries[0].Date != "2026-10-02" {
		t.Errorf("day = %+v err=%v", day, err)
	}
	if _, err := h.e.Timeline("b", "#nothing-here", 1, 1); err == nil {
		t.Error("an unknown anchor must be an error")
	}
}
