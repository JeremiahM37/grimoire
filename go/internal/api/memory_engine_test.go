package api

import (
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
