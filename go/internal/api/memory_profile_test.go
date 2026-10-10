package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// The profile. The deterministic selection is tested as a pure function, so
// ordering and budget are checked exactly; the endpoint tests cover what only
// the HTTP surface can get wrong: the cache, the subject checks and the model
// path's fallback.

func hit(id, text string, mut func(*index.MemoryHit)) index.MemoryHit {
	h := index.MemoryHit{Entry: memory.Entry{ID: id, Text: text, Agent: "probe",
		Stamp: time.Now().In(time.Local).Format(memory.StampFormat), Importance: 3},
		Note: "memory/" + id + ".md"}
	if mut != nil {
		mut(&h)
	}
	return h
}

func ids(doc profileDoc) []string {
	out := make([]string, 0, len(doc.Entries))
	for _, e := range doc.Entries {
		out = append(out, e.ID)
	}
	return out
}

func TestProfileRanksHumanFactsFirstThenByImportance(t *testing.T) {
	hits := []index.MemoryHit{
		hit("agentLow", "the agent noted something minor", func(h *index.MemoryHit) { h.Importance = 2 }),
		hit("agentHigh", "the build must stay green", func(h *index.MemoryHit) {
			h.Importance = 5
			h.Category = "constraint"
		}),
		hit("person", "I prefer short replies", func(h *index.MemoryHit) {
			h.Importance = 1
			h.Human = true
			h.Category = "preference"
		}),
	}
	doc := buildProfile(hits, "user", "", profileDefaultBudget, time.Now())

	if len(doc.Entries) != 3 {
		t.Fatalf("entries = %v", ids(doc))
	}
	if doc.Entries[0].ID != "person" {
		t.Errorf("a person's fact must come first even at importance 1, got %v", ids(doc))
	}
	if doc.Entries[1].ID != "agentHigh" || doc.Entries[2].ID != "agentLow" {
		t.Errorf("agent facts must rank by importance, got %v", ids(doc))
	}
	if !strings.HasPrefix(doc.Markdown, "## "+secHuman) {
		t.Errorf("the human section must be rendered first:\n%s", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "## "+secConstraints) {
		t.Errorf("a constraint must land in the constraints section:\n%s", doc.Markdown)
	}
}

func TestProfileHonoursItsTokenBudgetAndKeepsHumanFactsFirst(t *testing.T) {
	var hits []index.MemoryHit
	hits = append(hits, hit("person", "I prefer short replies", func(h *index.MemoryHit) { h.Human = true }))
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("fact%02d", i)
		hits = append(hits, hit(id, fmt.Sprintf("the deploy target number %d is staging-%d.internal", i, i),
			func(h *index.MemoryHit) { h.Importance = 3 }))
	}
	for _, budget := range []int{50, 120, 400} {
		doc := buildProfile(hits, "user", "", budget, time.Now())
		if got := bank.CountTokens(doc.Markdown); got > budget {
			t.Errorf("budget %d: markdown costs %d tokens", budget, got)
		}
		if doc.Tokens != bank.CountTokens(doc.Markdown) {
			t.Errorf("budget %d: reported %d tokens, counted %d", budget, doc.Tokens, bank.CountTokens(doc.Markdown))
		}
		if len(doc.Entries) == 0 || doc.Entries[0].ID != "person" {
			t.Errorf("budget %d: the human fact was not kept first: %v", budget, ids(doc))
		}
	}
}

func TestProfileLeavesOutSupersededDisputedExpiredAndPulledFacts(t *testing.T) {
	now := time.Now()
	old := now.Add(-time.Hour).In(time.Local).Format(memory.StampFormat)
	hits := []index.MemoryHit{
		hit("new", "the team uses postgres", nil),
		hit("old", "the team uses mysql", func(h *index.MemoryHit) {
			h.SupersededBy = "new"
			h.SupersededAt = now.Format(memory.StampFormat)
			h.Stamp = old
		}),
		hit("disputed", "the team uses sqlite", func(h *index.MemoryHit) { h.Challenges = "new" }),
		hit("pulled", "the team uses a fork of redis", func(h *index.MemoryHit) { h.Origin = "connector:web" }),
		hit("expired", "the team uses oracle", func(h *index.MemoryHit) {
			h.Expires = now.Add(-time.Minute).UTC().Format(time.RFC3339)
		}),
	}
	doc := buildProfile(hits, "user", "", profileDefaultBudget, now)

	standing := map[string]bool{}
	for _, e := range doc.Entries {
		if e.Section != secRecent {
			standing[e.ID] = true
		}
	}
	if !standing["new"] {
		t.Errorf("the live fact is missing: %v", ids(doc))
	}
	// Disputed and pulled facts are not news either, so they appear nowhere.
	for _, gone := range []string{"disputed", "pulled"} {
		if strings.Contains(doc.Markdown, gone) {
			t.Errorf("%s must not appear in a profile:\n%s", gone, doc.Markdown)
		}
	}
	// The expired fact is a recent change, so it may be listed there, and only there.
	if standing["expired"] {
		t.Errorf("an expired fact was listed as standing: %v", ids(doc))
	}
	// The replaced fact is not a standing fact, but the change it made is
	// recent, and the row says both sides of it.
	if standing["old"] {
		t.Errorf("a superseded fact was listed as standing: %v", ids(doc))
	}
	if !strings.Contains(doc.Markdown, "- [inferred] Changed: the team uses postgres (was: the team uses mysql) [mem:new] [mem:old]") {
		t.Errorf("the recent change is missing or uncited:\n%s", doc.Markdown)
	}
	if !strings.Contains(doc.Markdown, "## "+secRecent) {
		t.Errorf("no recent-changes section:\n%s", doc.Markdown)
	}
}

func TestEveryProfileLineCitesAFactItSelected(t *testing.T) {
	hits := []index.MemoryHit{
		hit("a1", "first fact", nil),
		hit("a2", "second fact", func(h *index.MemoryHit) { h.Human = true }),
		hit("a3", "third fact", func(h *index.MemoryHit) { h.Category = "preference" }),
	}
	doc := buildProfile(hits, "user", "", profileDefaultBudget, time.Now())
	allowed := map[string]bool{}
	for _, e := range doc.Entries {
		allowed[e.ID] = true
	}
	if err := validateCitations(doc.Markdown, allowed); err != nil {
		t.Fatalf("the deterministic profile does not pass its own citation check: %v\n%s", err, doc.Markdown)
	}
}

func TestProfileForAnAgentKeepsItsOwnFactsAndTheHumans(t *testing.T) {
	hits := []index.MemoryHit{
		hit("mine", "my own note", func(h *index.MemoryHit) { h.Agent = "claude-code" }),
		hit("theirs", "another agent's note", func(h *index.MemoryHit) { h.Agent = "codex" }),
		hit("person", "a person said this", func(h *index.MemoryHit) { h.Agent = "codex"; h.Human = true }),
	}
	doc := buildProfile(hits, "agent", "claude-code", profileDefaultBudget, time.Now())
	got := ids(doc)
	if len(got) != 2 || got[0] != "person" || got[1] != "mine" {
		t.Errorf("agent profile = %v, want the person's fact then the agent's own", got)
	}
}

func TestValidateCitationsRefusesUncitedAndForeignIDs(t *testing.T) {
	allowed := map[string]bool{"abc": true, "def": true}
	cases := []struct {
		name, text string
		ok         bool
	}{
		{"cited", "## Standing facts\n- uses postgres [mem:abc]\n- prefers short replies [mem:def]\n", true},
		{"uncited line", "- uses postgres [mem:abc]\n- prefers short replies\n", false},
		{"unknown id", "- uses postgres [mem:zzz]\n", false},
		{"one good, one bad", "- uses postgres [mem:abc] [mem:zzz]\n", false},
		{"only headings", "## Standing facts\n", false},
		{"empty", "   \n", false},
	}
	for _, tc := range cases {
		err := validateCitations(tc.text, allowed)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestProfileCacheHitsUntilTheMemoryMoves(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "text": "the team uses postgres", "agent": "probe"})

	first := profileOf(t, h, "/api/memory/profile")
	if first["cached"] != false {
		t.Fatalf("first read claims to be cached: %v", first["cached"])
	}
	second := profileOf(t, h, "/api/memory/profile")
	if second["cached"] != true {
		t.Fatalf("an unchanged memory should answer from the cache: %v", second["cached"])
	}
	if second["cursor"] != first["cursor"] {
		t.Errorf("cursor moved with no writes: %v -> %v", first["cursor"], second["cursor"])
	}

	remember(t, h, map[string]any{"topic": "deploy", "text": "deploys run on fridays", "agent": "probe"})
	third := profileOf(t, h, "/api/memory/profile")
	if third["cached"] != false {
		t.Fatal("a write did not invalidate the profile")
	}
	if !strings.Contains(fmt.Sprint(third["markdown"]), "deploys run on fridays") {
		t.Errorf("the new fact is missing after the write:\n%v", third["markdown"])
	}
}

func TestProfileModelRewriteIsKeptOnlyWhenItsCitationsCheck(t *testing.T) {
	s, h := testServer(t)
	id := fmt.Sprint(remember(t, h, map[string]any{"topic": "prefs", "text": "the team uses postgres", "agent": "probe"})["id"])

	// A rewrite that cites a fact that was never offered is refused, and the
	// verifiable selection comes back with the reason.
	s.profileModel = func(string) (string, error) {
		return "The team uses postgres [mem:" + id + "]\nIt also uses redis [mem:invented]\n", nil
	}
	bad := profileOf(t, h, "/api/memory/profile?synthesize=true")
	if bad["synthesized"] != false || bad["fallback"] != true {
		t.Fatalf("an invented citation was accepted: %v", bad)
	}
	if !strings.Contains(fmt.Sprint(bad["reason"]), "invented") {
		t.Errorf("reason does not name the bad id: %v", bad["reason"])
	}
	if bad["markdown"] != bad["deterministic"] || !strings.Contains(fmt.Sprint(bad["deterministic"]), "[mem:"+id+"]") {
		t.Errorf("the deterministic fallback is missing from the response: %v", bad)
	}

	// A different memory state, so the cache does not answer for us.
	remember(t, h, map[string]any{"topic": "deploy", "text": "deploys run on fridays", "agent": "probe"})
	s.profileModel = func(string) (string, error) {
		return "The team uses postgres [mem:" + id + "].\n", nil
	}
	good := profileOf(t, h, "/api/memory/profile?synthesize=true")
	if good["synthesized"] != true || good["fallback"] == true {
		t.Fatalf("a checked rewrite was refused: %v", good)
	}
	if !strings.Contains(fmt.Sprint(good["markdown"]), "The team uses postgres") {
		t.Errorf("the rewrite is not what was returned: %v", good["markdown"])
	}
}

func TestProfileRejectsBadParameters(t *testing.T) {
	_, h := testServer(t)
	for _, path := range []string{
		"/api/memory/profile?subject=stranger",
		"/api/memory/profile?budget=5",
		"/api/memory/profile?budget=lots",
		"/api/memory/profile?budget=99999",
	} {
		if w := do(t, h, "GET", path, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", path, w.Code)
		}
	}
}

func profileOf(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	w := do(t, h, "GET", path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d %s", path, w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	return out
}
