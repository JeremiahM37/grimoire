package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Importance over the API, usage from feedback, and prune. The eviction rules
// are tested here as the rules a person relies on: what is never removed, what
// a dry run leaves alone, and that an apply is an ordinary retraction.

func TestRememberStoresImportanceInTheBullet(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "text": "the backup key is in the vault",
		"infer": false, "importance": 5})
	body := noteBody(t, h, "memory/ops.md")
	if !strings.Contains(body, "imp=5") {
		t.Fatalf("importance not written to the bullet:\n%s", body)
	}
}

func TestRememberWithoutImportanceWritesNoField(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "text": "the cache is redis", "infer": false})
	if body := noteBody(t, h, "memory/ops.md"); strings.Contains(body, "imp=") {
		t.Fatalf("an unrated fact was given an importance:\n%s", body)
	}
}

func TestRememberRejectsImportanceOutsideOneToFive(t *testing.T) {
	_, h := testServer(t)
	for _, bad := range []int{-1, 6, 9} {
		w := do(t, h, "POST", "/api/memory", map[string]any{
			"topic": "ops", "text": "a fact", "infer": false, "importance": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("importance %d = %d, want 400", bad, w.Code)
		}
	}
}

func TestAReplacementKeepsItsPredecessorsImportance(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs",
		"infer": false, "importance": 5})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces",
		"target_id": out["id"], "target_path": "memory/prefs.md",
		"expected_text": "the user prefers tabs"})
	body := noteBody(t, h, "memory/prefs.md")
	if !strings.Contains(body, "the user prefers spaces") || !strings.Contains(body, "imp=5") {
		t.Fatalf("replacement did not inherit importance 5:\n%s", body)
	}
}

func TestHelpfulFeedbackCountsUseInTheIndexOnly(t *testing.T) {
	s, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "ops", "text": "the deploy host is prod-1", "infer": false})
	id := out["id"].(string)

	if w := do(t, h, "POST", "/api/memory/feedback", map[string]any{
		"path": "memory/ops.md", "id": id, "helpful": false}); w.Code != http.StatusOK {
		t.Fatalf("unhelpful = %d: %s", w.Code, w.Body)
	}
	if hits, _ := s.Index.MemoryEntries(indexQuery()); hits[0].Uses != 0 {
		t.Fatalf("an unhelpful vote counted as a use: %d", hits[0].Uses)
	}

	if w := do(t, h, "POST", "/api/memory/feedback", map[string]any{
		"path": "memory/ops.md", "id": id, "helpful": true}); w.Code != http.StatusOK {
		t.Fatalf("helpful = %d: %s", w.Code, w.Body)
	}
	hits, err := s.Index.MemoryEntries(indexQuery())
	if err != nil {
		t.Fatal(err)
	}
	if hits[0].Uses != 1 || hits[0].LastUsed == "" {
		t.Errorf("uses = %d last_used = %q, want 1 and set", hits[0].Uses, hits[0].LastUsed)
	}
	if body := noteBody(t, h, "memory/ops.md"); strings.Contains(body, "uses") || strings.Contains(body, "last_used") {
		t.Errorf("usage leaked into the markdown:\n%s", body)
	}
}

func indexQuery() index.MemoryQuery { return index.MemoryQuery{Limit: 50, Now: vault.Now()} }

// pruneRun posts to the prune endpoint and decodes the response.
func pruneRun(t *testing.T, h http.Handler, body map[string]any) map[string]any {
	t.Helper()
	w := do(t, h, "POST", "/api/memory/prune", body)
	if w.Code != http.StatusOK {
		t.Fatalf("prune %v = %d: %s", body, w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	return out
}

// backdated writes one fact as if it had been written that many days ago.
func backdated(t *testing.T, h http.Handler, days int, body map[string]any) {
	t.Helper()
	when := vault.Now().Add(-time.Duration(days) * 24 * time.Hour)
	at(t, when, func() { remember(t, h, body) })
}

func TestPruneDryRunIsTheDefaultAndChangesNothing(t *testing.T) {
	_, h := testServer(t)
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the mailer is postfix",
		"infer": false, "importance": 1})
	before := noteBody(t, h, "memory/ops.md")

	out := pruneRun(t, h, map[string]any{})
	if out["apply"] != false {
		t.Fatalf("default run applied: %v", out)
	}
	if cands, _ := out["candidates"].([]any); len(cands) != 1 {
		t.Fatalf("dry run candidates = %v, want the one old low fact", out["candidates"])
	}
	if after := noteBody(t, h, "memory/ops.md"); after != before {
		t.Fatalf("a dry run changed the note:\n%s", after)
	}
}

func TestPruneApplyRetractsThroughTheForgetPath(t *testing.T) {
	_, h := testServer(t)
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the mailer is postfix",
		"infer": false, "importance": 1})

	out := pruneRun(t, h, map[string]any{"apply": true})
	if out["removed"] != float64(1) {
		t.Fatalf("removed = %v, want 1", out["removed"])
	}
	body := noteBody(t, h, "memory/ops.md")
	if !strings.Contains(body, "retracted:memory-prune") {
		t.Errorf("retraction not attributed to memory-prune:\n%s", body)
	}
	if !strings.Contains(body, "the mailer is postfix") {
		t.Errorf("eviction deleted the record instead of striking it through:\n%s", body)
	}
	if facts := recallFacts(t, h, ""); len(facts) != 0 {
		t.Errorf("evicted fact still recalled: %q", texts(facts))
	}
}

func TestPruneNeverTakesAProtectedFact(t *testing.T) {
	_, h := testServer(t)
	// Every one of these is old and low on its face. None may be evicted.
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the database is postgres",
		"infer": false, "importance": 4})
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the backup key is in the vault",
		"infer": false, "importance": 5})
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the cache is memcached",
		"infer": false})
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the proxy is caddy",
		"infer": false, "importance": 1, "human": true})
	backdated(t, h, 200, map[string]any{"topic": "ops", "text": "the search is meili",
		"infer": false, "importance": 1, "immutable": true})
	backdated(t, h, 10, map[string]any{"topic": "ops", "text": "the queue is rabbit",
		"infer": false, "importance": 1})

	out := pruneRun(t, h, map[string]any{"apply": true, "below": 3, "max": 100})
	if out["removed"] != float64(0) {
		t.Fatalf("removed = %v, want 0: every fact here is protected", out["removed"])
	}
	if cands, _ := out["candidates"].([]any); len(cands) != 0 {
		t.Errorf("protected facts listed as candidates: %v", cands)
	}
	if strings.Contains(noteBody(t, h, "memory/ops.md"), "retracted:") {
		t.Error("a protected fact was retracted")
	}
}

func TestPruneInputIsValidated(t *testing.T) {
	_, h := testServer(t)
	for _, body := range []map[string]any{
		{"below": 4},  // 4 and 5 are never candidates, so asking for them is an error
		{"below": -1}, // not a level
		{"max": 501},  // over the cap
		{"max": -2},
	} {
		if w := do(t, h, "POST", "/api/memory/prune", body); w.Code != http.StatusBadRequest {
			t.Errorf("prune %v = %d, want 400", body, w.Code)
		}
	}
}

func TestPruneEligibilityMirrorsTheProtections(t *testing.T) {
	cutoff := vault.Now().Add(-90 * 24 * time.Hour)
	old := cutoff.Add(-24 * time.Hour).Format(memory.StampFormat)
	fresh := vault.Now().Format(memory.StampFormat)
	base := memory.Entry{ID: "x", Text: "t", Agent: "claude", Stamp: old, Importance: 1}
	if !pruneEligible(base, 2, cutoff) {
		t.Fatal("the plain candidate was refused")
	}
	cases := map[string]func(e *memory.Entry){
		"human":      func(e *memory.Entry) { e.Human = true },
		"hand":       func(e *memory.Entry) { e.HandWritten = true },
		"immutable":  func(e *memory.Entry) { e.Immutable = true },
		"superseded": func(e *memory.Entry) { e.SupersededBy = "y" },
		"challenged": func(e *memory.Entry) { e.Challenges = "y" },
		"voted":      func(e *memory.Entry) { e.Helpful = 1 },
		"unrated":    func(e *memory.Entry) { e.Importance = 0 },
		"rated 4":    func(e *memory.Entry) { e.Importance = 4 },
		"rated 5":    func(e *memory.Entry) { e.Importance = 5 },
		"no agent":   func(e *memory.Entry) { e.Agent = "" },
		"too recent": func(e *memory.Entry) { e.Stamp = fresh },
	}
	for name, mutate := range cases {
		e := base
		mutate(&e)
		if pruneEligible(e, 3, cutoff) {
			t.Errorf("%s: a protected fact was eligible for eviction", name)
		}
	}
}
