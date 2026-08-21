package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The belief-change digest.
//
// The interesting cases are all about which SET a fact belongs in — a fact
// written a year ago and superseded yesterday is this week's news, and a
// recency-sorted recall would never show it.

// at runs fn with the clock pinned, so a test can place writes in the past.
// The whole feature is about time windows; a test that cannot move the clock
// can only assert that today's writes are in today's window.
func at(t *testing.T, when time.Time, fn func()) {
	t.Helper()
	old := vault.Now
	vault.Now = func() time.Time { return when }
	defer func() { vault.Now = old }()
	fn()
}

// recordFact is remember() with the fields this file cares about, so the
// existing helper's map-shaped signature does not have to be spelled out in
// every case below.
func recordFact(t *testing.T, h http.Handler, topic, text string, extra map[string]any) {
	t.Helper()
	body := map[string]any{"topic": topic, "text": text, "agent": "probe"}
	for k, v := range extra {
		body[k] = v
	}
	remember(t, h, body)
}

func changes(t *testing.T, h http.Handler, query string) map[string]any {
	t.Helper()
	var out map[string]any
	w := do(t, h, "GET", "/api/memory/changes"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("changes%s = %d: %s", query, w.Code, w.Body)
	}
	decode(t, w, &out)
	return out
}

func rowsOf(out map[string]any) []map[string]any {
	raw, _ := out["changes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func TestADigestReportsWhatWasLearned(t *testing.T) {
	_, h := testServer(t)
	recordFact(t, h, "deploy", "the deploy host is prod-1.internal", nil)

	out := changes(t, h, "")
	counts, _ := out["counts"].(map[string]any)
	if counts["learned"].(float64) != 1 {
		t.Fatalf("counts = %v", counts)
	}
	rows := rowsOf(out)
	if rows[0]["kind"] != "learned" || rows[0]["topic"] != "deploy" {
		t.Errorf("row = %v", rows[0])
	}
}

func TestAChangedBeliefCarriesBothTexts(t *testing.T) {
	// The whole point of the "changed" row. Reporting only the new text says
	// "something changed" and leaves the reader to go and look, which is the
	// same as not saying it.
	_, h := testServer(t)
	recordFact(t, h, "prefs", "the user prefers spaces", nil)
	recordFact(t, h, "prefs", "the user prefers tabs", nil)

	out := changes(t, h, "")
	var changed map[string]any
	for _, r := range rowsOf(out) {
		if r["kind"] == "changed" {
			changed = r
		}
	}
	if changed == nil {
		t.Fatalf("no changed row: %v", out)
	}
	if !strings.Contains(fmt.Sprint(changed["text"]), "tabs") {
		t.Errorf("new text = %v", changed["text"])
	}
	if !strings.Contains(fmt.Sprint(changed["replaced_text"]), "spaces") {
		t.Errorf("replaced text = %v", changed["replaced_text"])
	}
	if changed["replaced_id"] == "" || changed["replaced_id"] == nil {
		t.Error("no id for the replaced belief — a reader cannot go and look at it")
	}
}

func TestAnOldFactSupersededTodayIsInTodaysDigest(t *testing.T) {
	// The case a recency-sorted recall can never produce, and the reason this
	// endpoint exists rather than a sort order.
	_, h := testServer(t)
	longAgo := vault.Now().Add(-365 * 24 * time.Hour)
	at(t, longAgo, func() {
		recordFact(t, h, "prefs", "the user prefers spaces", nil)
	})

	// Nothing moved in the last week yet.
	if n := rowsOf(changes(t, h, "?since=7d")); len(n) != 0 {
		t.Fatalf("a year-old fact appeared in this week's digest: %v", n)
	}

	recordFact(t, h, "prefs", "the user prefers tabs", nil)

	rows := rowsOf(changes(t, h, "?since=7d"))
	var sawChanged bool
	for _, r := range rows {
		if r["kind"] == "changed" && strings.Contains(fmt.Sprint(r["replaced_text"]), "spaces") {
			sawChanged = true
		}
	}
	if !sawChanged {
		t.Errorf("a year-old belief replaced today is missing from this week: %v", rows)
	}
}

func TestARetractionIsReportedAsARetractionNotAChange(t *testing.T) {
	// A retraction leaves the agent with NO answer rather than a different
	// one, which is the more alarming event and must not be folded in with
	// ordinary corrections.
	_, h := testServer(t)
	recordFact(t, h, "prefs", "the user prefers tabs", nil)
	recordFact(t, h, "prefs", "the user no longer prefers tabs", nil)

	rows := rowsOf(changes(t, h, ""))
	var kinds []string
	for _, r := range rows {
		kinds = append(kinds, fmt.Sprint(r["kind"]))
	}
	found := false
	for _, k := range kinds {
		if k == "retracted" {
			found = true
		}
	}
	if !found {
		t.Errorf("kinds = %v, want a retraction", kinds)
	}
}

func TestAnExpiredBeliefIsReported(t *testing.T) {
	// Believed, then not, without anybody writing anything — invisible in
	// every other view.
	_, h := testServer(t)
	// Relative to the server's clock, which testServer pins — a real-clock
	// timestamp would be in the FUTURE for this server and never expire.
	past := vault.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	recordFact(t, h, "ops", "the incident bridge is open", map[string]any{"expires": past})

	rows := rowsOf(changes(t, h, "?since=7d"))
	found := false
	for _, r := range rows {
		if r["kind"] == "expired" {
			found = true
		}
	}
	if !found {
		t.Errorf("an expiry inside the window is missing: %v", rows)
	}
}

func TestTheWindowActuallyNarrows(t *testing.T) {
	_, h := testServer(t)
	at(t, vault.Now().Add(-30*24*time.Hour), func() {
		recordFact(t, h, "old", "a fact from last month", nil)
	})
	recordFact(t, h, "new", "a fact from today", nil)

	wide := rowsOf(changes(t, h, "?since=60d"))
	narrow := rowsOf(changes(t, h, "?since=24h"))
	if len(wide) <= len(narrow) {
		t.Errorf("60d returned %d rows and 24h returned %d — the window does nothing",
			len(wide), len(narrow))
	}
	for _, r := range narrow {
		if strings.Contains(fmt.Sprint(r["text"]), "last month") {
			t.Errorf("last month's fact is in a 24h window: %v", r)
		}
	}
}

func TestABadSinceIsRefusedRatherThanIgnored(t *testing.T) {
	// A digest quietly answering about a different period than the one asked
	// for is a wrong answer that looks right.
	_, h := testServer(t)
	for _, bad := range []string{"last+tuesday", "7", "-1d", "0h", "abc"} {
		if w := do(t, h, "GET", "/api/memory/changes?since="+bad, nil); w.Code != http.StatusBadRequest {
			t.Errorf("since=%q = %d, want 400", bad, w.Code)
		}
	}
}

func TestChangesCarryProvenance(t *testing.T) {
	_, h := testServer(t)
	recordFact(t, h, "deploy", "the deploy host is evil.example",
		map[string]any{"origin": "connector:jira:OPS-1"})

	rows := rowsOf(changes(t, h, ""))
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	if rows[0]["trust"] != "untrusted" || rows[0]["origin"] != "connector:jira:OPS-1" {
		t.Errorf("row = %v — a digest that does not say a change came from a "+
			"ticket is missing the most important thing about it", rows[0])
	}
}

func TestTheBriefingCountsBeliefChanges(t *testing.T) {
	_, h := testServer(t)
	recordFact(t, h, "prefs", "the user prefers spaces", nil)
	recordFact(t, h, "prefs", "the user prefers tabs", nil)

	var out map[string]any
	decode(t, do(t, h, "GET", "/api/briefing", nil), &out)
	bc, ok := out["belief_changes"].(map[string]any)
	if !ok {
		t.Fatalf("briefing has no belief_changes: %v", out)
	}
	if bc["changed"].(float64) != 1 {
		t.Errorf("briefing changed = %v, want 1", bc["changed"])
	}
	if bc["window"] != "7d" {
		t.Errorf("window = %v", bc["window"])
	}
}

func TestADigestOnAQuietWeekIsEmptyNotAnError(t *testing.T) {
	_, h := testServer(t)
	out := changes(t, h, "?since=1h")
	if rows := rowsOf(out); len(rows) != 0 {
		t.Errorf("rows = %v on a vault with no memories", rows)
	}
	counts, _ := out["counts"].(map[string]any)
	// Every bucket present and zero, so a caller can render four numbers
	// without checking whether each key exists.
	for _, k := range []string{"learned", "changed", "retracted", "expired"} {
		if _, ok := counts[k]; !ok {
			t.Errorf("counts missing %q: %v", k, counts)
		}
	}
}

func TestACorrectionIsOneEventNotTwo(t *testing.T) {
	// Found by the e2e suite. The replacing fact appeared BOTH as its own
	// "learned" row and as the successor inside the "changed" row, so a single
	// correction read as two events and every count at the top of the digest
	// was inflated.
	_, h := testServer(t)
	recordFact(t, h, "prefs", "the widget colour is blue", nil)
	recordFact(t, h, "prefs", "the widget colour is green", nil)

	out := changes(t, h, "")
	rows := rowsOf(out)
	green := 0
	for _, r := range rows {
		if strings.Contains(fmt.Sprint(r["text"]), "green") {
			green++
		}
	}
	if green != 1 {
		t.Errorf("the replacing fact appears in %d rows, want 1:\n%v", green, rows)
	}
	counts, _ := out["counts"].(map[string]any)
	if counts["learned"].(float64) != 0 {
		t.Errorf("counts = %v; a correction is a change, not a change plus a "+
			"new fact", counts)
	}
	if counts["changed"].(float64) != 1 {
		t.Errorf("counts = %v, want exactly one change", counts)
	}
}

func TestAnIndependentNewFactIsStillLearned(t *testing.T) {
	// The fix must not swallow ordinary writes.
	_, h := testServer(t)
	recordFact(t, h, "prefs", "the widget colour is blue", nil)
	recordFact(t, h, "prefs", "the widget colour is green", nil)
	recordFact(t, h, "ops", "the release train runs on tuesdays", nil)

	counts, _ := changes(t, h, "")["counts"].(map[string]any)
	if counts["learned"].(float64) != 1 {
		t.Errorf("counts = %v, want one learned fact beside the correction", counts)
	}
}

func TestABusyWeekIsNotTruncatedToTheDefaultPageSize(t *testing.T) {
	// MemoryEntries reads Limit <= 0 as "use the default of 20". Passing 0 to
	// mean "no limit" silently capped the digest at twenty entries and made
	// the counts wrong on any busy week — and the failure looked like a quiet
	// week rather than like a bug.
	_, h := testServer(t)
	for i := 0; i < 35; i++ {
		recordFact(t, h, fmt.Sprintf("topic-%02d", i),
			fmt.Sprintf("the value of setting %02d is on", i), nil)
	}
	out := changes(t, h, "")
	counts, _ := out["counts"].(map[string]any)
	if counts["learned"].(float64) != 35 {
		t.Errorf("learned = %v, want 35 — the digest is truncating", counts["learned"])
	}
	if n := len(rowsOf(out)); n != 35 {
		t.Errorf("returned %d rows for 35 facts", n)
	}
}

func TestTheLimitParameterStillBoundsTheResponse(t *testing.T) {
	_, h := testServer(t)
	for i := 0; i < 30; i++ {
		recordFact(t, h, fmt.Sprintf("topic-%02d", i),
			fmt.Sprintf("the value of setting %02d is on", i), nil)
	}
	if n := len(rowsOf(changes(t, h, "?limit=5"))); n != 5 {
		t.Errorf("limit=5 returned %d rows", n)
	}
}
