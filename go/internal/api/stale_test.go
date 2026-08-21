package api

import (
	"net/http"
	"testing"
)

// The review queue: which notes a person should actually go and re-check.

func writeVerified(t *testing.T, h http.Handler, path, verified, body string) {
	t.Helper()
	fm := map[string]any{}
	if verified != "" {
		fm["verified"] = verified
	}
	w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": path, "body": body, "frontmatter": fm})
	if w.Code != http.StatusCreated {
		t.Fatalf("creating %s = %d: %s", path, w.Code, w.Body)
	}
}

func staleQueue(t *testing.T, h http.Handler, query string) map[string]any {
	t.Helper()
	var out map[string]any
	w := do(t, h, "GET", "/api/stale"+query, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("stale%s = %d: %s", query, w.Code, w.Body)
	}
	decode(t, w, &out)
	return out
}

func staleRows(out map[string]any) []map[string]any {
	raw, _ := out["notes"].([]any)
	rows := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			rows = append(rows, m)
		}
	}
	return rows
}

func TestTheQueueListsOnlyOverdueNotes(t *testing.T) {
	_, h := testServer(t)
	writeVerified(t, h, "rotten.md", "2019-01-01", "# Rotten\n\nan old runbook")
	writeVerified(t, h, "fine.md", "2026-08-20", "# Fine\n\nchecked yesterday")

	rows := staleRows(staleQueue(t, h, ""))
	if len(rows) != 1 {
		t.Fatalf("queue = %v, want just the rotten note", rows)
	}
	if rows[0]["path"] != "rotten.md" {
		t.Errorf("row = %v", rows[0])
	}
	if rows[0]["verified"] != true {
		t.Errorf("a note with an explicit verified date reported verified=%v",
			rows[0]["verified"])
	}
}

func TestWhatTheVaultLeansOnComesFirst(t *testing.T) {
	// Both overdue; the one a dozen notes point at is the one worth a person's
	// afternoon.
	_, h := testServer(t)
	writeVerified(t, h, "hub.md", "2020-06-01", "# Hub\n\nthe deploy runbook")
	writeVerified(t, h, "corner.md", "2020-01-01", "# Corner\n\nsomething nobody references")
	for i := 0; i < 6; i++ {
		writeVerified(t, h, "ref"+string(rune('a'+i))+".md", "2026-08-20",
			"see [[Hub]] for the procedure")
	}

	rows := staleRows(staleQueue(t, h, ""))
	if len(rows) < 2 {
		t.Fatalf("queue = %v", rows)
	}
	if rows[0]["path"] != "hub.md" {
		t.Errorf("queue order = %v; the note everything links to should be first",
			rows[0]["path"])
	}
	if rows[0]["inbound"].(float64) < 6 {
		t.Errorf("inbound = %v, want the backlinks counted", rows[0]["inbound"])
	}
}

func TestTheQueueReportsTheWholeBacklogNotJustThePage(t *testing.T) {
	// A queue that says "20" when there are 340 is how a backlog stays
	// invisible.
	_, h := testServer(t)
	for i := 0; i < 25; i++ {
		writeVerified(t, h, "old"+string(rune('a'+i))+".md", "2019-01-01", "old note")
	}
	out := staleQueue(t, h, "?limit=5")
	if len(staleRows(out)) != 5 {
		t.Errorf("returned %d rows for limit=5", len(staleRows(out)))
	}
	if out["total"].(float64) != 25 {
		t.Errorf("total = %v, want 25", out["total"])
	}
}

func TestMemoryNotesAreNotInTheQueue(t *testing.T) {
	// Facts carry their own lifecycle — TTL, decay, supersession — so asking a
	// person to re-verify a memory note is busywork that teaches them to
	// ignore the queue.
	_, h := testServer(t)
	remember(t, h, map[string]any{
		"topic": "deploy", "agent": "probe", "text": "the deploy host is prod-1"})
	out := staleQueue(t, h, "?days=0")
	for _, row := range staleRows(out) {
		if p, _ := row["path"].(string); len(p) > 7 && p[:7] == "memory/" {
			t.Errorf("a memory note is in the review queue: %v", row)
		}
	}
}

func TestTheThresholdIsAdjustable(t *testing.T) {
	_, h := testServer(t)
	writeVerified(t, h, "recent.md", "2026-08-01", "# Recent\n\nchecked three weeks ago")

	if rows := staleRows(staleQueue(t, h, "")); len(rows) != 0 {
		t.Fatalf("a three-week-old note is overdue at the default threshold: %v", rows)
	}
	if rows := staleRows(staleQueue(t, h, "?days=7")); len(rows) != 1 {
		t.Errorf("days=7 returned %v, want the three-week-old note", rows)
	}
	if w := do(t, h, "GET", "/api/stale?days=not-a-number", nil); w.Code != http.StatusBadRequest {
		t.Errorf("days=not-a-number = %d, want 400", w.Code)
	}
}

func TestTheQueueSaysHowMuchOfTheVaultHasEverBeenReviewed(t *testing.T) {
	// On a vault where nobody uses `verified:`, the queue is really an age
	// listing. Saying so is more honest than letting it look like a review
	// process that is running.
	_, h := testServer(t)
	writeVerified(t, h, "never.md", "", "nobody has confirmed this")
	writeVerified(t, h, "once.md", "2026-01-01", "somebody confirmed this")

	out := staleQueue(t, h, "?days=0")
	if out["reviewed"].(float64) != 1 {
		t.Errorf("reviewed = %v, want 1 of 2", out["reviewed"])
	}
}

func TestConfirmingANoteWritesTheDateIntoTheFile(t *testing.T) {
	s, h := testServer(t)
	writeVerified(t, h, "rotten.md", "2019-01-01", "# Rotten\n\nan old runbook")
	if len(staleRows(staleQueue(t, h, ""))) != 1 {
		t.Fatal("fixture is not in the queue")
	}

	w := do(t, h, "POST", "/api/stale/verify", map[string]any{"path": "rotten.md"})
	if w.Code != http.StatusOK {
		t.Fatalf("verify = %d: %s", w.Code, w.Body)
	}

	note, err := s.Vault.Read("rotten.md")
	if err != nil {
		t.Fatal(err)
	}
	if note.Frontmatter.StringVal("verified") == "" {
		t.Errorf("the confirmation is not in the file: %q", note.Raw)
	}
	if rows := staleRows(staleQueue(t, h, "")); len(rows) != 0 {
		t.Errorf("a just-confirmed note is still in the queue: %v", rows)
	}
}

func TestConfirmingWithAnUnreadableDateIsRefused(t *testing.T) {
	// A date the parser will not read would leave the note carrying a
	// `verified:` line and still counted as never checked — the most confusing
	// possible outcome.
	_, h := testServer(t)
	writeVerified(t, h, "rotten.md", "2019-01-01", "old")
	w := do(t, h, "POST", "/api/stale/verify",
		map[string]any{"path": "rotten.md", "date": "last tuesday"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("verify with a bad date = %d, want 400: %s", w.Code, w.Body)
	}
}

func TestConfirmingAMissingNoteIs404(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/stale/verify",
		map[string]any{"path": "nope.md"}); w.Code != http.StatusNotFound {
		t.Errorf("verify missing = %d, want 404", w.Code)
	}
}

func TestRetrievalHitsCarryAgeAndStaleness(t *testing.T) {
	_, h := testServer(t)
	writeVerified(t, h, "rotten.md", "2019-01-01", "# Rotten\n\nthe kestrel deploy runbook")

	var hits []map[string]any
	decode(t, do(t, h, "GET", "/api/retrieve?q=kestrel+deploy&k=5", nil), &hits)
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0]["stale"] != true {
		t.Errorf("hit = %v, want stale", hits[0])
	}
	if hits[0]["age_days"].(float64) < 365 {
		t.Errorf("age_days = %v", hits[0]["age_days"])
	}
}

func TestPrivateNotesAreInTheReviewQueue(t *testing.T) {
	// `private` excludes a note from retrieval; it is not an access boundary,
	// and the note list already shows private notes to whoever may see the
	// space. Excluding them from the queue would hide exactly the runbooks
	// most worth re-checking from the person who owns them.
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "secret-runbook.md", "body": "# Secret runbook\n\nan old private procedure",
		"frontmatter": map[string]any{"verified": "2019-01-01", "private": true}})
	if w.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", w.Code, w.Body)
	}
	rows := staleRows(staleQueue(t, h, ""))
	found := false
	for _, r := range rows {
		if r["path"] == "secret-runbook.md" {
			found = true
		}
	}
	if !found {
		t.Errorf("a private note that is years old is missing from the queue: %v", rows)
	}
}
