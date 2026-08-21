package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The behaviour of the memory engine, end to end over HTTP: what a write does
// to what is already known, what recall returns, and what the note on disk
// looks like afterwards. The rules themselves are unit-tested in
// internal/memory; these are about the wiring — that a decision reaches the
// vault, the index, the history, and the response.

func remember(t *testing.T, h http.Handler, body map[string]any) map[string]any {
	t.Helper()
	w := do(t, h, "POST", "/api/memory", body)
	if w.Code >= 400 {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	return out
}

func recallFacts(t *testing.T, h http.Handler, query string) []map[string]any {
	t.Helper()
	w := do(t, h, "GET", "/api/memory"+query, nil)
	if w.Code >= 400 {
		t.Fatalf("recall = %d: %s", w.Code, w.Body)
	}
	var out []map[string]any
	decode(t, w, &out)
	return out
}

func noteBody(t *testing.T, h http.Handler, path string) string {
	t.Helper()
	w := do(t, h, "GET", "/api/notes/"+path, nil)
	if w.Code >= 400 {
		t.Fatalf("read %s = %d: %s", path, w.Code, w.Body)
	}
	var note map[string]any
	decode(t, w, &note)
	body, _ := note["body"].(string)
	return body
}

func texts(facts []map[string]any) []string {
	out := make([]string, len(facts))
	for i, f := range facts {
		out[i], _ = f["text"].(string)
	}
	return out
}

func TestContradictionSupersedesTheOldBelief(t *testing.T) {
	_, h := testServer(t)
	first := remember(t, h, map[string]any{
		"topic": "prefs", "text": "the user prefers spaces", "agent": "claude"})
	if first["op"] != "ADD" {
		t.Fatalf("first write = %v, want ADD", first["op"])
	}
	second := remember(t, h, map[string]any{
		"topic": "prefs", "text": "the user prefers tabs", "agent": "claude"})
	if second["op"] != "UPDATE" {
		t.Fatalf("contradiction = %v, want UPDATE: %v", second["op"], second)
	}
	if second["results"].([]any)[0].(map[string]any)["target"] != first["id"] {
		t.Errorf("superseded the wrong entry: %v", second)
	}

	// Recall returns only what is currently believed.
	facts := recallFacts(t, h, "?q=indentation+preference")
	for _, f := range texts(facts) {
		if strings.Contains(f, "spaces") {
			t.Errorf("a superseded belief was recalled: %q", texts(facts))
		}
	}
	if len(facts) == 0 || !strings.Contains(texts(facts)[0], "tabs") {
		t.Errorf("current belief not recalled: %q", texts(facts))
	}

	// But the note keeps it, struck through — the record of what the agent
	// used to believe is the point of storing memory in a file.
	body := noteBody(t, h, "memory/prefs.md")
	if !strings.Contains(body, "prefers spaces") {
		t.Errorf("the old belief was deleted rather than superseded:\n%s", body)
	}
	if !strings.Contains(body, "~~") {
		t.Errorf("the old belief is not struck through:\n%s", body)
	}
}

func TestSupersededBeliefIsRecoverableOnRequest(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})

	all := recallFacts(t, h, "?include_superseded=1")
	if len(all) != 2 {
		t.Fatalf("include_superseded returned %q, want both", texts(all))
	}
	var found bool
	for _, f := range all {
		if strings.Contains(f["text"].(string), "spaces") && f["superseded_by"] != "" {
			found = true
		}
	}
	if !found {
		t.Errorf("the superseded entry does not name what replaced it: %v", all)
	}
}

func TestRetractionRemovesTheBeliefWithoutReplacingIt(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	out := remember(t, h, map[string]any{
		"topic": "prefs", "text": "the user no longer prefers tabs"})
	if out["op"] != "DELETE" {
		t.Fatalf("retraction = %v, want DELETE: %v", out["op"], out)
	}
	if facts := recallFacts(t, h, "?q=tabs"); len(facts) != 0 {
		t.Errorf("a retracted belief was recalled: %q", texts(facts))
	}
	// The retraction itself is not stored as a belief — "no longer prefers
	// tabs" recalled as a fact would be its own kind of wrong.
	body := noteBody(t, h, "memory/prefs.md")
	if strings.Count(body, "no longer prefers tabs") > 0 &&
		!strings.Contains(body, "~~") {
		t.Errorf("retraction stored as a live fact:\n%s", body)
	}
}

func TestRestatementIsNotWrittenTwice(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "text": "the deploy needs a VPN reset"})
	out := remember(t, h, map[string]any{"topic": "ops", "text": "The deploy needs a VPN reset."})
	if out["op"] != "NOOP" {
		t.Fatalf("restatement = %v, want NOOP: %v", out["op"], out)
	}
	body := noteBody(t, h, "memory/ops.md")
	if n := strings.Count(body, "deploy needs a VPN reset"); n != 1 {
		t.Errorf("belief stored %d times:\n%s", n, body)
	}
}

func TestUnrelatedFactsAccumulate(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "text": "the user prefers tabs"})
	remember(t, h, map[string]any{"topic": "ops", "text": "the backup runs at three in the morning"})
	facts := recallFacts(t, h, "")
	if len(facts) != 2 {
		t.Fatalf("unrelated facts were merged: %q", texts(facts))
	}
}

func TestSupersessionIsRollbackable(t *testing.T) {
	// A rewrite of an agent's memory that a person cannot undo is a rewrite
	// they have to trust blindly.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})

	var versions []map[string]any
	decode(t, do(t, h, "GET", "/api/notes/memory/prefs.md/history", nil), &versions)
	if len(versions) == 0 {
		t.Fatal("no snapshot taken before the belief was superseded")
	}
}

func TestReconciliationCrossesNotes(t *testing.T) {
	// The contradicted belief does not have to be in the note being written.
	_, h := testServer(t)
	first := remember(t, h, map[string]any{
		"topic": "old-notes", "text": "the user prefers spaces"})
	second := remember(t, h, map[string]any{
		"topic": "new-notes", "text": "the user prefers tabs"})
	if second["op"] != "UPDATE" {
		t.Fatalf("cross-note contradiction = %v: %v", second["op"], second)
	}
	if !strings.Contains(noteBody(t, h, "memory/old-notes.md"), "~~") {
		t.Errorf("the other note's belief was not struck through")
	}
	_ = first
}

func TestImmutableFactIsNeverSuperseded(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{
		"topic": "rules", "text": "the user prefers tabs", "immutable": true})
	out := remember(t, h, map[string]any{"topic": "rules", "text": "the user prefers spaces"})
	if out["op"] != "ADD" {
		t.Fatalf("a pinned fact was reconciled away: %v", out)
	}
	facts := recallFacts(t, h, "")
	if len(facts) != 2 {
		t.Errorf("want both the pinned fact and the new one, got %q", texts(facts))
	}
}

func TestInferFalseStoresVerbatim(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "raw", "text": "the user prefers tabs"})
	out := remember(t, h, map[string]any{
		"topic": "raw", "text": "the user prefers spaces", "infer": false})
	if out["op"] != "ADD" {
		t.Fatalf("infer=false reconciled anyway: %v", out)
	}
	if facts := recallFacts(t, h, ""); len(facts) != 2 {
		t.Errorf("want both stored verbatim, got %q", texts(facts))
	}
}

func TestScopesAreStoredAndFilterable(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "work", "text": "alice owns the release",
		"agent": "alice-bot", "session": "run-1", "category": "ownership"})
	remember(t, h, map[string]any{"topic": "work", "text": "the build takes nine minutes",
		"agent": "bob-bot", "session": "run-2", "category": "timing"})

	for query, want := range map[string]string{
		"?agent=alice-bot":       "alice owns the release",
		"?session=run-2":         "the build takes nine minutes",
		"?category=ownership":    "alice owns the release",
		"?path=memory/work.md&q": "",
	} {
		facts := recallFacts(t, h, query)
		if want == "" {
			if len(facts) != 2 {
				t.Errorf("%s returned %q, want both", query, texts(facts))
			}
			continue
		}
		if len(facts) != 1 || facts[0]["text"] != want {
			t.Errorf("%s returned %q, want [%q]", query, texts(facts), want)
		}
	}
}

func TestSessionScopeAnswersWhatWasLearnedInThisRun(t *testing.T) {
	_, h := testServer(t)
	for _, text := range []string{"the api key rotates monthly", "the staging box is smaller"} {
		remember(t, h, map[string]any{"topic": "run", "text": text, "session": "run-42"})
	}
	remember(t, h, map[string]any{"topic": "run", "text": "unrelated older fact", "session": "run-7"})

	facts := recallFacts(t, h, "?session=run-42")
	if len(facts) != 2 {
		t.Fatalf("session scope returned %q", texts(facts))
	}
	for _, f := range facts {
		if f["session"] != "run-42" {
			t.Errorf("wrong session leaked in: %v", f)
		}
	}
}

func TestExpiryStopsRecallWithoutDeleting(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "oncall",
		"text": "priya is on call this week", "expires_in": "1h"})
	if facts := recallFacts(t, h, ""); len(facts) != 1 {
		t.Fatalf("fact did not survive its own lifetime: %q", texts(facts))
	}
	// The clock is frozen at the test's "now", so an already-past expiry is
	// the way to observe the other side of it.
	remember(t, h, map[string]any{"topic": "oncall",
		"text": "marco was on call last week", "expires": "2026-08-13T00:00:00Z"})
	facts := recallFacts(t, h, "")
	if len(facts) != 1 || !strings.Contains(facts[0]["text"].(string), "priya") {
		t.Errorf("expired fact was recalled: %q", texts(facts))
	}
	if !strings.Contains(noteBody(t, h, "memory/oncall.md"), "marco") {
		t.Error("an expired fact was deleted rather than hidden")
	}
	if all := recallFacts(t, h, "?include_expired=1"); len(all) != 2 {
		t.Errorf("include_expired returned %q", texts(all))
	}
}

func TestExpiryValidation(t *testing.T) {
	_, h := testServer(t)
	for _, body := range []map[string]any{
		{"text": "x", "expires": "next tuesday"},
		{"text": "x", "expires_in": "soon"},
		{"text": "x", "expires_in": "-3h"},
		{"text": "x", "expires": "2026-09-01T00:00:00Z", "expires_in": "3h"},
	} {
		if w := do(t, h, "POST", "/api/memory", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, w.Code)
		}
	}
}

func TestScopeFieldsAreValidated(t *testing.T) {
	// They are matched exactly in SQL and rendered into the bullet's trailer,
	// where a space would truncate the field and a '>' would close the comment.
	_, h := testServer(t)
	for _, body := range []map[string]any{
		{"text": "x", "session": "run 1"},
		{"text": "x", "session": "a-->b"},
		{"text": "x", "category": "two words"},
	} {
		if w := do(t, h, "POST", "/api/memory", body); w.Code != http.StatusBadRequest {
			t.Errorf("%v = %d, want 400", body, w.Code)
		}
	}
}

func TestExplainShowsWhyAFactWasRecalled(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "team", "text": "Priya owns the deploy script"})
	facts := recallFacts(t, h, "?q=who+owns+the+deploy+script&explain=1")
	if len(facts) == 0 {
		t.Fatal("nothing recalled")
	}
	scores, ok := facts[0]["scores"].(map[string]any)
	if !ok {
		t.Fatalf("no score breakdown: %v", facts[0])
	}
	for _, k := range []string{"semantic", "keyword", "entity", "recency"} {
		if _, ok := scores[k]; !ok {
			t.Errorf("breakdown missing %q: %v", k, scores)
		}
	}
	// And it is off by default: an agent asking for facts should get facts.
	plain := recallFacts(t, h, "?q=deploy")
	if _, ok := plain[0]["scores"]; ok {
		t.Error("score breakdown returned without being asked for")
	}
}

func TestPatchEntryEditsOneFactInPlace(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	id := out["id"].(string)

	w := do(t, h, "PATCH", "/api/memory/entry", map[string]any{
		"path": "memory/prefs.md", "id": id,
		"text": "the user prefers four-space indentation", "category": "style"})
	if w.Code != http.StatusOK {
		t.Fatalf("patch = %d: %s", w.Code, w.Body)
	}
	facts := recallFacts(t, h, "")
	if len(facts) != 1 || facts[0]["text"] != "the user prefers four-space indentation" {
		t.Fatalf("edit not applied: %q", texts(facts))
	}
	if facts[0]["category"] != "style" {
		t.Errorf("category not applied: %v", facts[0])
	}
	if facts[0]["id"] != id {
		t.Errorf("editing a fact changed its identity: %v", facts[0])
	}
}

func TestPatchEntryLeavesTheRestOfTheNoteAlone(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "the office is on the third floor"})

	do(t, h, "PATCH", "/api/memory/entry", map[string]any{
		"path": "memory/prefs.md", "id": out["id"], "text": "the office is on the fourth floor"})

	body := noteBody(t, h, "memory/prefs.md")
	if !strings.Contains(body, "the user prefers tabs") {
		t.Errorf("editing one fact disturbed another:\n%s", body)
	}
	if !strings.Contains(body, "# Memory: prefs") {
		t.Errorf("the note's heading was lost:\n%s", body)
	}
}

func TestPatchEntryRejectsUnknownAndNonMemoryTargets(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "a fact"})
	if w := do(t, h, "PATCH", "/api/memory/entry", map[string]any{
		"path": "memory/prefs.md", "id": "nope", "text": "x"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", w.Code)
	}
	do(t, h, "POST", "/api/notes", map[string]any{"title": "Plain", "body": "# Plain\n"})
	if w := do(t, h, "PATCH", "/api/memory/entry", map[string]any{
		"path": "plain.md", "id": "x", "text": "y"}); w.Code != http.StatusBadRequest {
		t.Errorf("non-memory note = %d, want 400", w.Code)
	}
	if w := do(t, h, "PATCH", "/api/memory/entry",
		map[string]any{"id": "x"}); w.Code != http.StatusBadRequest {
		t.Errorf("missing path = %d, want 400", w.Code)
	}
}

func TestForgetRetractsButKeepsTheRecord(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "a mistaken belief"})
	w := do(t, h, "DELETE",
		"/api/memory/entry?path=memory/prefs.md&id="+out["id"].(string), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("forget = %d: %s", w.Code, w.Body)
	}
	if facts := recallFacts(t, h, ""); len(facts) != 0 {
		t.Errorf("retracted fact still recalled: %q", texts(facts))
	}
	if !strings.Contains(noteBody(t, h, "memory/prefs.md"), "a mistaken belief") {
		t.Error("a soft retraction deleted the record")
	}
}

func TestHardForgetRemovesTheLine(t *testing.T) {
	// The case where the fact itself is the problem and striking it through is
	// not an answer.
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "something private"})
	w := do(t, h, "DELETE",
		"/api/memory/entry?path=memory/prefs.md&id="+out["id"].(string)+"&hard=1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("hard forget = %d: %s", w.Code, w.Body)
	}
	if body := noteBody(t, h, "memory/prefs.md"); strings.Contains(body, "something private") {
		t.Errorf("hard forget left the text behind:\n%s", body)
	}
	if facts := recallFacts(t, h, "?include_superseded=1&include_expired=1"); len(facts) != 0 {
		t.Errorf("hard forget left an index row: %q", texts(facts))
	}
	// Still recoverable by rollback until history rotates.
	var versions []map[string]any
	decode(t, do(t, h, "GET", "/api/notes/memory/prefs.md/history", nil), &versions)
	if len(versions) == 0 {
		t.Error("hard forget took no snapshot")
	}
}

func TestForgetRejectsUnknownEntry(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "a fact"})
	if w := do(t, h, "DELETE",
		"/api/memory/entry?path=memory/prefs.md&id=nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", w.Code)
	}
	if w := do(t, h, "DELETE", "/api/memory/entry?id=x", nil); w.Code != http.StatusBadRequest {
		t.Errorf("missing path = %d, want 400", w.Code)
	}
}

func TestBatchReconcilesAgainstItsOwnEarlierItems(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory/batch", map[string]any{"items": []map[string]any{
		{"topic": "batch", "text": "the user prefers spaces"},
		{"topic": "batch", "text": "the build takes nine minutes"},
		{"topic": "batch", "text": "the user prefers tabs"},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("batch = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Results []map[string]any `json:"results"`
	}
	decode(t, w, &out)
	if len(out.Results) != 3 {
		t.Fatalf("got %d results, want 3", len(out.Results))
	}
	if out.Results[2]["op"] != "UPDATE" {
		t.Errorf("the third item did not supersede the first: %v", out.Results[2])
	}
	facts := recallFacts(t, h, "")
	if len(facts) != 2 {
		t.Errorf("batch left %q on file, want the two live facts", texts(facts))
	}
}

func TestBatchValidation(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/memory/batch",
		map[string]any{"items": []any{}}); w.Code != http.StatusBadRequest {
		t.Errorf("empty batch = %d, want 400", w.Code)
	}
	big := make([]map[string]any, 101)
	for i := range big {
		big[i] = map[string]any{"text": "x"}
	}
	if w := do(t, h, "POST", "/api/memory/batch",
		map[string]any{"items": big}); w.Code != http.StatusBadRequest {
		t.Errorf("oversized batch = %d, want 400", w.Code)
	}
}

func TestBatchReportsPerItemFailure(t *testing.T) {
	// One bad item must not lose the good ones, and must not be reported as a
	// success.
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory/batch", map[string]any{"items": []map[string]any{
		{"topic": "batch", "text": "a perfectly good fact"},
		{"topic": "batch", "text": ""},
	}})
	var out struct {
		Results []map[string]any `json:"results"`
	}
	decode(t, w, &out)
	if len(out.Results) != 2 {
		t.Fatalf("got %d results", len(out.Results))
	}
	if out.Results[0]["status"].(float64) != http.StatusCreated {
		t.Errorf("good item = %v", out.Results[0])
	}
	if out.Results[1]["status"].(float64) != http.StatusBadRequest {
		t.Errorf("bad item reported as success: %v", out.Results[1])
	}
	if facts := recallFacts(t, h, ""); len(facts) != 1 {
		t.Errorf("good item lost: %q", texts(facts))
	}
}

func TestExportReturnsEverythingIncludingHistory(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})

	var out struct {
		Count   int              `json:"count"`
		Entries []map[string]any `json:"entries"`
	}
	decode(t, do(t, h, "GET", "/api/memory/export", nil), &out)
	if out.Count != 2 || len(out.Entries) != 2 {
		t.Fatalf("export = %d entries, want 2 (including the superseded one)", out.Count)
	}
}

func TestFacetsListTheScopesInUse(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "w", "text": "one fact",
		"agent": "alice-bot", "session": "run-1", "category": "ownership"})
	remember(t, h, map[string]any{"topic": "w", "text": "another separate fact",
		"agent": "alice-bot", "session": "run-2", "category": "timing"})

	var out struct {
		Agents     map[string]int `json:"agents"`
		Sessions   map[string]int `json:"sessions"`
		Categories map[string]int `json:"categories"`
		Total      int            `json:"total"`
		Live       int            `json:"live"`
	}
	decode(t, do(t, h, "GET", "/api/memory/facets", nil), &out)
	if out.Agents["alice-bot"] != 2 {
		t.Errorf("agents = %v", out.Agents)
	}
	if len(out.Sessions) != 2 || len(out.Categories) != 2 {
		t.Errorf("sessions = %v categories = %v", out.Sessions, out.Categories)
	}
	if out.Total != 2 || out.Live != 2 {
		t.Errorf("total = %d live = %d, want 2 and 2", out.Total, out.Live)
	}
}

func TestBriefingCarriesCurrentFacts(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})

	var out struct {
		RecentFacts []map[string]any `json:"recent_facts"`
	}
	decode(t, do(t, h, "GET", "/api/briefing", nil), &out)
	if len(out.RecentFacts) != 1 {
		t.Fatalf("briefing carried %q, want only the current belief", texts(out.RecentFacts))
	}
	if !strings.Contains(out.RecentFacts[0]["text"].(string), "tabs") {
		t.Errorf("briefing carried the superseded belief: %v", out.RecentFacts[0])
	}
}

func TestFactRecallRespectsReaderLists(t *testing.T) {
	// The fact-level surface is a new way to read note content, and every new
	// way to read note content is a new way to leak it.
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if _, err := s.WriteNote(MemoryDir+"/restricted.md",
		"# Memory: restricted\n\n- **2026-08-14 09:00 · claude** — the severance terms are confidential <!--m id=r1-->\n",
		map[string]any{"title": "Restricted", "memory": true, "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	// An open memory note in the same folder, so "bob sees nothing" cannot
	// pass by the surface being broken for everyone.
	if _, err := s.WriteNote(MemoryDir+"/open.md",
		"# Memory: open\n\n- **2026-08-14 09:00 · claude** — the standup is at nine <!--m id=o1-->\n",
		map[string]any{"title": "Open", "memory": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/api/memory?q=terms", "/api/memory/export", "/api/memory/facets"} {
		body := asKey(t, h, bobKey, "GET", path, nil).Body.String()
		if strings.Contains(strings.ToLower(body), "severance") {
			t.Errorf("%s leaked a restricted fact to a member not on the list:\n%s", path, body)
		}
	}
	if body := asKey(t, h, bobKey, "GET", "/api/memory?q=standup", nil).Body.String(); !strings.Contains(body, "standup") {
		t.Errorf("the open fact was hidden too: %s", body)
	}
	if body := asKey(t, h, aliceKey, "GET", "/api/memory?q=terms", nil).Body.String(); !strings.Contains(body, "severance") {
		t.Errorf("the person named on the reader list cannot see her own fact: %s", body)
	}
}

func TestReconciliationCannotSupersedeAcrossAReaderList(t *testing.T) {
	// Writing a contradicting fact must not become a way to strike through a
	// belief in a note the writer may not touch.
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if _, err := s.WriteNote(MemoryDir+"/alice-only.md",
		"# Memory: alice\n\n- **2026-08-14 09:00 · claude** — the user prefers spaces <!--m id=a1-->\n",
		map[string]any{"title": "Alice", "memory": true, "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	w := asKey(t, h, bobKey, "POST", "/api/memory",
		map[string]any{"topic": "bobs-notes", "text": "the user prefers tabs"})
	if w.Code >= 500 {
		t.Fatalf("write = %d: %s", w.Code, w.Body)
	}
	body := asKey(t, h, aliceKey, "GET", "/api/notes/memory/alice-only.md", nil).Body.String()
	if strings.Contains(body, "~~") {
		t.Errorf("a member struck through a fact in a note they cannot read:\n%s", body)
	}
}

func TestRecallLimitIsBounded(t *testing.T) {
	_, h := testServer(t)
	for i := 0; i < 5; i++ {
		remember(t, h, map[string]any{"topic": "many",
			"text": "distinct fact number " + string(rune('a'+i)), "infer": false})
	}
	if facts := recallFacts(t, h, "?limit=2"); len(facts) != 2 {
		t.Errorf("limit ignored: %d", len(facts))
	}
	if facts := recallFacts(t, h, "?limit=0"); len(facts) != 1 {
		t.Errorf("limit=0 should clamp to 1, got %d", len(facts))
	}
	if facts := recallFacts(t, h, "?limit=99999"); len(facts) != 5 {
		t.Errorf("oversized limit = %d", len(facts))
	}
}

func TestMemoryEndpointsRejectAnonymousWrites(t *testing.T) {
	// The route audit records the intent; this checks the handlers.
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin")
	_ = adminKey
	for _, tc := range []struct{ method, path string }{
		{"POST", "/api/memory"},
		{"POST", "/api/memory/batch"},
		{"PATCH", "/api/memory/entry"},
		{"DELETE", "/api/memory/entry?path=memory/x.md&id=1"},
	} {
		req := requestFor(t, tc.method, tc.path, map[string]any{
			"text": "x", "path": "memory/x.md", "id": "1",
			"items": []map[string]any{{"text": "x"}}})
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s = %d, want 401", tc.method, tc.path, w.Code)
		}
	}
}

func TestBatchThatWritesNothingIsNotASuccess(t *testing.T) {
	// A caller checking only the HTTP status must not record a write that
	// never happened.
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory/batch", map[string]any{"items": []map[string]any{
		{"text": ""}, {"text": "x", "session": "not valid"},
	}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("all-failed batch = %d, want 400: %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	if out["written"].(float64) != 0 || out["failed"].(float64) != 2 {
		t.Errorf("counts = %v", out)
	}
}

func TestPartialBatchStaysASuccessAndSaysWhatLanded(t *testing.T) {
	_, h := testServer(t)
	w := do(t, h, "POST", "/api/memory/batch", map[string]any{"items": []map[string]any{
		{"topic": "b", "text": "a fact that lands"}, {"text": ""},
	}})
	if w.Code != http.StatusOK {
		t.Fatalf("partial batch = %d, want 200", w.Code)
	}
	var out map[string]any
	decode(t, w, &out)
	if out["written"].(float64) != 1 || out["failed"].(float64) != 1 {
		t.Errorf("counts = %v", out)
	}
}

func TestAsOfReconstructsAnEarlierBelief(t *testing.T) {
	// The question a store that deletes what it replaces cannot answer.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})

	// The test clock is frozen at 2026-08-14 09:00, so both writes carry that
	// stamp; an instant before them has nothing, and one after has the current
	// belief only.
	if facts := recallFacts(t, h, "?as_of=2026-08-13T09:00:00Z"); len(facts) != 0 {
		t.Errorf("facts existed before they were written: %q", texts(facts))
	}
	facts := recallFacts(t, h, "?as_of=2026-08-20T09:00:00Z")
	if len(facts) != 1 || !strings.Contains(facts[0]["text"].(string), "tabs") {
		t.Errorf("as_of after the change returned %q, want the current belief", texts(facts))
	}
}

func TestAsOfRejectsAMalformedInstant(t *testing.T) {
	// Answering a historical question about the present is a wrong answer that
	// looks right.
	_, h := testServer(t)
	if w := do(t, h, "GET", "/api/memory?as_of=last+tuesday", nil); w.Code != http.StatusBadRequest {
		t.Errorf("as_of=last tuesday = %d, want 400", w.Code)
	}
}

func TestSupersessionRecordsWhenItHappened(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers spaces"})
	remember(t, h, map[string]any{"topic": "prefs", "text": "the user prefers tabs"})
	if !strings.Contains(noteBody(t, h, "memory/prefs.md"), "supat=") {
		t.Errorf("supersession has no timestamp:\n%s", noteBody(t, h, "memory/prefs.md"))
	}
}

func TestRetractionRecordsWhenItHappened(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "prefs", "text": "a mistaken belief"})
	do(t, h, "DELETE", "/api/memory/entry?path=memory/prefs.md&id="+out["id"].(string), nil)
	if !strings.Contains(noteBody(t, h, "memory/prefs.md"), "supat=") {
		t.Error("retraction has no timestamp")
	}
}

func TestRecallFiltersByTask(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "w", "text": "the first fact", "task": "ticket-4"})
	remember(t, h, map[string]any{"topic": "w", "text": "the second fact", "task": "ticket-9"})

	facts := recallFacts(t, h, "?task=ticket-9")
	if len(facts) != 1 || facts[0]["text"] != "the second fact" {
		t.Fatalf("task filter returned %q", texts(facts))
	}
}

func TestTopicScopeKeepsNamespacesFromSupersedingEachOther(t *testing.T) {
	// The isolation a partitioned store depends on: a write into one topic
	// must not be able to strike through a belief in another.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "alice", "text": "the user prefers spaces"})
	out := remember(t, h, map[string]any{
		"topic": "bob", "text": "the user prefers tabs", "scope": "topic"})
	if out["op"] != "ADD" {
		t.Fatalf("a scoped write reached another topic: %v", out)
	}
	if strings.Contains(noteBody(t, h, "memory/alice.md"), "~~") {
		t.Error("another topic's belief was struck through")
	}
	// Within its own topic it still reconciles.
	same := remember(t, h, map[string]any{
		"topic": "bob", "text": "the user prefers four-space indentation", "scope": "topic"})
	if same["op"] != "UPDATE" {
		t.Errorf("topic scope stopped reconciliation inside the topic: %v", same)
	}
}

func TestSessionScopeIsolatesRuns(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "w", "text": "the user prefers spaces",
		"session": "run-1", "scope": "session"})
	out := remember(t, h, map[string]any{"topic": "w", "text": "the user prefers tabs",
		"session": "run-2", "scope": "session"})
	if out["op"] != "ADD" {
		t.Fatalf("a run superseded another run's belief: %v", out)
	}
	same := remember(t, h, map[string]any{"topic": "w",
		"text":    "the user prefers four-space indentation",
		"session": "run-2", "scope": "session"})
	if same["op"] != "UPDATE" {
		t.Errorf("session scope stopped reconciliation inside the run: %v", same)
	}
}

func TestSessionScopeWithNoSessionOnlyReachesSessionlessFacts(t *testing.T) {
	// "" must mean "the facts with no session", not "every session".
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "w", "text": "the user prefers spaces",
		"session": "run-1"})
	out := remember(t, h, map[string]any{"topic": "w", "text": "the user prefers tabs",
		"scope": "session"})
	if out["op"] != "ADD" {
		t.Fatalf("a sessionless write superseded a session's belief: %v", out)
	}
}

func TestAgentScopeIsolatesAgents(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "w", "text": "the user prefers spaces",
		"agent": "alice-bot", "scope": "agent"})
	out := remember(t, h, map[string]any{"topic": "w", "text": "the user prefers tabs",
		"agent": "bob-bot", "scope": "agent"})
	if out["op"] != "ADD" {
		t.Fatalf("one agent superseded another's belief: %v", out)
	}
}

func TestDefaultScopeStillReconcilesAcrossTheVault(t *testing.T) {
	// The default has to stay "the whole vault": for one person's memory, a
	// belief contradicted in another note is still contradicted.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "one", "text": "the user prefers spaces"})
	out := remember(t, h, map[string]any{"topic": "two", "text": "the user prefers tabs"})
	if out["op"] != "UPDATE" {
		t.Errorf("the default scope stopped reconciling across notes: %v", out)
	}
}

func TestInvalidScopeIsRefused(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/memory",
		map[string]any{"text": "x", "scope": "everything"}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown scope = %d, want 400", w.Code)
	}
}

func TestFeedbackCountsAndReordersWithoutBurying(t *testing.T) {
	_, h := testServer(t)
	// Two facts that both answer the query and do NOT reconcile against each
	// other — otherwise the engine correctly supersedes one and there is
	// nothing left to reorder.
	good := remember(t, h, map[string]any{"topic": "ops",
		"text": "restart gluetun when downloads stall"})
	bad := remember(t, h, map[string]any{"topic": "ops",
		"text": "restart the router when downloads stall"})

	for i := 0; i < 3; i++ {
		w := do(t, h, "POST", "/api/memory/feedback", map[string]any{
			"path": "memory/ops.md", "id": bad["id"], "helpful": false})
		if w.Code != http.StatusOK {
			t.Fatalf("feedback = %d: %s", w.Code, w.Body)
		}
	}
	w := do(t, h, "POST", "/api/memory/feedback", map[string]any{
		"path": "memory/ops.md", "id": good["id"], "helpful": true})
	var out map[string]any
	decode(t, w, &out)
	if out["helpful"].(float64) != 1 {
		t.Errorf("counter not incremented: %v", out)
	}

	facts := recallFacts(t, h, "?q=what+to+restart+when+downloads+stall")
	if len(facts) != 2 {
		t.Fatalf("feedback removed a fact: %q", texts(facts))
	}
	if !strings.Contains(facts[0]["text"].(string), "gluetun") {
		t.Errorf("feedback did not reorder: %q", texts(facts))
	}
	// Bounded: the disliked fact still ranks, because it may still be the only
	// answer to some other question.
	if facts[1]["score"].(float64) <= 0 {
		t.Errorf("a disliked fact was buried: %v", facts[1])
	}
}

func TestFeedbackSurvivesInTheNoteAndTheBreakdown(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "ops", "text": "a fact worth keeping"})
	do(t, h, "POST", "/api/memory/feedback", map[string]any{
		"path": "memory/ops.md", "id": out["id"], "helpful": true})

	if body := noteBody(t, h, "memory/ops.md"); !strings.Contains(body, "up=1") {
		t.Errorf("feedback is not in the note, so it is not rebuildable:\n%s", body)
	}
	facts := recallFacts(t, h, "?q=fact&explain=1")
	if facts[0]["helpful"].(float64) != 1 {
		t.Errorf("recall does not report the count: %v", facts[0])
	}
	scores := facts[0]["scores"].(map[string]any)
	if _, ok := scores["useful"]; !ok {
		t.Errorf("breakdown does not show the feedback component: %v", scores)
	}
}

func TestFeedbackValidation(t *testing.T) {
	_, h := testServer(t)
	out := remember(t, h, map[string]any{"topic": "ops", "text": "a fact"})
	for _, body := range []map[string]any{
		{"path": "memory/ops.md", "id": out["id"]}, // no verdict
		{"id": out["id"], "helpful": true},         // no path
		{"path": "memory/ops.md", "helpful": true}, // no id
		{"path": "memory/ops.md", "id": "nope", "helpful": true},
		{"path": "plain.md", "id": "x", "helpful": true},
	} {
		if w := do(t, h, "POST", "/api/memory/feedback", body); w.Code < 400 {
			t.Errorf("%v = %d, want a refusal", body, w.Code)
		}
	}
}

func TestFeedbackNeedsWriteAccessToTheFactsNote(t *testing.T) {
	// The answer to "is this a lever on someone else's ranking": no, because
	// it is a write to the note the fact lives in.
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote(MemoryDir+"/alice-only.md",
		"# Memory: alice\n\n- **2026-08-14 09:00 · claude** — alice's fact <!--m id=a1-->\n",
		map[string]any{"title": "Alice", "memory": true, "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	w := asKey(t, h, bobKey, "POST", "/api/memory/feedback", map[string]any{
		"path": "memory/alice-only.md", "id": "a1", "helpful": false})
	if w.Code < 400 {
		t.Fatalf("a member voted on a fact they cannot read: %d", w.Code)
	}
	body := asKey(t, h, aliceKey, "GET", "/api/notes/memory/alice-only.md", nil).Body.String()
	if strings.Contains(body, "down=") {
		t.Error("the vote landed anyway")
	}
}

func TestGraphOverHTTPCarriesNodesEdgesAndEvidence(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "team",
		"text": "Priya Sharma and Marco Diaz maintain AIServer"})
	remember(t, h, map[string]any{"topic": "team",
		"text": "Marco Diaz owns the Deploy Runbook"})

	var g struct {
		Seed  string `json:"seed"`
		Nodes []struct {
			Entity string `json:"entity"`
			Facts  int    `json:"facts"`
			Depth  int    `json:"depth"`
		} `json:"nodes"`
		Edges []struct {
			From, To string
			Facts    []string
		} `json:"edges"`
		Entries []map[string]any `json:"entries"`
	}
	decode(t, do(t, h, "GET", "/api/memory/graph?entity=priya&depth=2", nil), &g)
	if g.Seed != "priya sharma" {
		t.Fatalf("seed = %q", g.Seed)
	}
	var names []string
	for _, n := range g.Nodes {
		names = append(names, n.Entity)
	}
	for _, want := range []string{"marco diaz", "aiserver", "deploy runbook"} {
		if !containsString(names, want) {
			t.Errorf("graph missing %q: %v", want, names)
		}
	}
	if len(g.Edges) == 0 || len(g.Edges[0].Facts) == 0 {
		t.Fatalf("edges carry no evidence: %+v", g.Edges)
	}
	if len(g.Entries) == 0 {
		t.Error("the graph did not carry the facts behind it")
	}
}

func TestGraphWithoutASeedIsAnOverview(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "team", "text": "Marco Diaz owns the Deploy Runbook"})
	var g struct {
		Nodes []struct{ Entity string } `json:"nodes"`
	}
	decode(t, do(t, h, "GET", "/api/memory/graph", nil), &g)
	if len(g.Nodes) == 0 {
		t.Fatal("overview returned nothing")
	}
}

func TestGraphDepthAndLimitAreBounded(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "team", "text": "Marco Diaz owns the Deploy Runbook"})
	for _, query := range []string{"?entity=marco&depth=99", "?entity=marco&depth=-1",
		"?limit=99999", "?limit=0"} {
		if w := do(t, h, "GET", "/api/memory/graph"+query, nil); w.Code != http.StatusOK {
			t.Errorf("%s = %d", query, w.Code)
		}
	}
}

func TestGraphRespectsReaderLists(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote(MemoryDir+"/restricted.md",
		"# Memory\n\n- **2026-08-14 09:00 · claude** — Priya Sharma runs the Severance Project <!--m id=r1-->\n",
		map[string]any{"title": "R", "memory": true, "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	body := asKey(t, h, bobKey, "GET", "/api/memory/graph?entity=priya&depth=2", nil).Body.String()
	if strings.Contains(strings.ToLower(body), "severance") {
		t.Errorf("the graph leaked a restricted entity:\n%s", body)
	}
	body = asKey(t, h, aliceKey, "GET", "/api/memory/graph?entity=priya&depth=2", nil).Body.String()
	if !strings.Contains(strings.ToLower(body), "severance") {
		t.Errorf("the graph hid it from the person named on the list:\n%s", body)
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestEmbedReturnsVectorsInTheServersSpace(t *testing.T) {
	_, h := testServer(t)
	var out struct {
		Model      string      `json:"model"`
		Dimensions int         `json:"dimensions"`
		Embeddings [][]float32 `json:"embeddings"`
	}
	decode(t, do(t, h, "POST", "/api/embed",
		map[string]any{"texts": []string{"one", "two"}}), &out)
	if len(out.Embeddings) != 2 {
		t.Fatalf("got %d embeddings, want 2", len(out.Embeddings))
	}
	if len(out.Embeddings[0]) != out.Dimensions {
		t.Errorf("vector has %d values, reported %d dimensions",
			len(out.Embeddings[0]), out.Dimensions)
	}
	// The signature identifies the space, so a caller caching vectors can tell
	// when the model changed underneath them.
	if out.Model == "" {
		t.Error("no model signature")
	}
	// The single-text form, for the common case.
	decode(t, do(t, h, "POST", "/api/embed", map[string]any{"text": "one"}), &out)
	if len(out.Embeddings) != 1 {
		t.Errorf("single text returned %d embeddings", len(out.Embeddings))
	}
}

func TestEmbedValidation(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/embed", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Errorf("empty = %d, want 400", w.Code)
	}
	big := make([]string, 257)
	for i := range big {
		big[i] = "x"
	}
	if w := do(t, h, "POST", "/api/embed",
		map[string]any{"texts": big}); w.Code != http.StatusBadRequest {
		t.Errorf("oversized = %d, want 400", w.Code)
	}
}

func TestVectorSearchRoundTripsThroughEmbed(t *testing.T) {
	// The whole point: embed here, search here, one space.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "facts",
		"text": "the deploy script lives at /usr/local/bin", "infer": false})
	remember(t, h, map[string]any{"topic": "facts",
		"text": "the cat is named marmalade", "infer": false})

	var emb struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	decode(t, do(t, h, "POST", "/api/embed",
		map[string]any{"text": "the deploy script lives at /usr/local/bin"}), &emb)

	var hits []map[string]any
	decode(t, do(t, h, "POST", "/api/memory/search",
		map[string]any{"embedding": emb.Embeddings[0], "limit": 5}), &hits)
	if len(hits) == 0 {
		t.Fatal("vector search returned nothing")
	}
	if !strings.Contains(hits[0]["text"].(string), "deploy script") {
		t.Errorf("wrong fact ranked first: %q", texts(hits))
	}
}

func TestVectorSearchRefusesAForeignEmbeddingSpace(t *testing.T) {
	// The check that keeps this honest. Cosine does not report that it is
	// comparing two unrelated coordinate systems; it reports a number, and
	// retrieval looks like it works.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "facts", "text": "a fact"})
	w := do(t, h, "POST", "/api/memory/search",
		map[string]any{"embedding": []float32{0.1, 0.2, 0.3}})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("a foreign vector was accepted: %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/api/embed") {
		t.Errorf("the refusal does not say how to fix it: %s", w.Body)
	}
}

func TestVectorSearchAppliesTheSameFiltersAndAccess(t *testing.T) {
	s, h := testServer(t)
	remember(t, h, map[string]any{"topic": "facts", "text": "run one fact",
		"session": "run-1", "infer": false})
	remember(t, h, map[string]any{"topic": "facts", "text": "run two fact",
		"session": "run-2", "infer": false})
	var emb struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	decode(t, do(t, h, "POST", "/api/embed", map[string]any{"text": "fact"}), &emb)

	var hits []map[string]any
	decode(t, do(t, h, "POST", "/api/memory/search", map[string]any{
		"embedding": emb.Embeddings[0], "session": "run-2"}), &hits)
	if len(hits) != 1 || !strings.Contains(hits[0]["text"].(string), "run two") {
		t.Fatalf("session filter not applied: %q", texts(hits))
	}

	// And the reader list, since this is another way to read note content.
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	if _, err := s.WriteNote(MemoryDir+"/restricted.md",
		"# Memory\n\n- **2026-08-14 09:00 · claude** — the severance terms are confidential <!--m id=r1-->\n",
		map[string]any{"title": "R", "memory": true, "readers": "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	body := asKey(t, h, bobKey, "POST", "/api/memory/search", map[string]any{
		"embedding": emb.Embeddings[0], "limit": 50}).Body.String()
	if strings.Contains(strings.ToLower(body), "severance") {
		t.Errorf("vector search leaked a restricted fact:\n%s", body)
	}
}

func TestVectorSearchNeedsSomethingToSearchWith(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/memory/search",
		map[string]any{"limit": 5}); w.Code != http.StatusBadRequest {
		t.Errorf("empty search = %d, want 400", w.Code)
	}
}

// The value-slot rule reaching the engine through HTTP, and staying inside the
// bounds every other supersession respects.
func TestAValueUpdateSupersedesThroughTheAPI(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "run", "agent": "probe",
		"text": "I set a personal best time in the charity 5K run with a time of 27:12"})
	out := remember(t, h, map[string]any{"topic": "run", "agent": "probe",
		"text": "I'm hoping to beat my personal best time of 25:50 this time around"})
	if out["op"] != "UPDATE" {
		t.Fatalf("op = %v (%v) — a numeric correction was stored beside the "+
			"fact it corrects", out["op"], out["results"])
	}

	// Recall returns the CURRENT value only. That is the whole point: the
	// reader is not handed two numbers and asked to pick.
	var facts []map[string]any
	decode(t, do(t, h, "GET", "/api/memory?q=personal+best+5K+time&limit=10", nil), &facts)
	var texts []string
	for _, f := range facts {
		texts = append(texts, fmt.Sprint(f["text"]))
	}
	joined := strings.Join(texts, " | ")
	if !strings.Contains(joined, "25:50") {
		t.Errorf("current value missing from recall: %s", joined)
	}
	if strings.Contains(joined, "27:12") {
		t.Errorf("superseded value is still recalled: %s", joined)
	}

	// …and the old value is still IN the note, struck through, because that is
	// what makes an agent's correction reviewable.
	var note map[string]any
	decode(t, do(t, h, "GET", "/api/notes/memory/run.md", nil), &note)
	if !strings.Contains(fmt.Sprint(note["body"]), "27:12") {
		t.Errorf("the superseded value was deleted rather than struck through")
	}
}

func TestAValueUpdateIsReportedInTheBeliefDigest(t *testing.T) {
	// A numeric correction is exactly the kind of change a person wants to see
	// in the weekly digest, and it only appears there if it reconciled.
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "run", "agent": "probe",
		"text": "I set a personal best time in the charity 5K run with a time of 27:12"})
	remember(t, h, map[string]any{"topic": "run", "agent": "probe",
		"text": "I'm hoping to beat my personal best time of 25:50 this time around"})

	var out map[string]any
	decode(t, do(t, h, "GET", "/api/memory/changes?since=1d", nil), &out)
	body := fmt.Sprint(out)
	if !strings.Contains(body, "changed") || !strings.Contains(body, "27:12") {
		t.Errorf("the digest does not show the correction:\n%s", body)
	}
}
