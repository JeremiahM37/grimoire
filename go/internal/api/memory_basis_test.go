package api

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// Basis, evidence and the factual/personal split, over the HTTP surface. The
// unit rules live in memory/basis_test.go; these tests pin what a caller sees.

// basisOf returns the basis of the recalled fact whose text contains needle.
func basisOf(t *testing.T, facts []map[string]any, needle string) string {
	t.Helper()
	for _, f := range facts {
		if strings.Contains(f["text"].(string), needle) {
			b, _ := f["basis"].(string)
			return b
		}
	}
	t.Fatalf("no recalled fact contains %q in %v", needle, facts)
	return ""
}

func TestBasisAppearsOnEveryRecalledFact(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "text": "the deploy host is prod-1", "infer": false})

	facts := recallFacts(t, h, "?q=deploy")
	if len(facts) == 0 {
		t.Fatal("recall returned nothing")
	}
	if b := basisOf(t, facts, "deploy host"); b != "inferred" {
		t.Fatalf("a bare agent write read as %q, want inferred", b)
	}
}

func TestEvidencePromotesAnAgentFactToObserved(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text":     "the backup runs at 03:00",
		"evidence": []string{"memory/cron.md", "https://example.org/runbook"}})

	body := noteBody(t, h, "memory/ops.md")
	if !strings.Contains(body, "ev=memory/cron.md,https://example.org/runbook") {
		t.Fatalf("evidence not written into the trailer:\n%s", body)
	}
	facts := recallFacts(t, h, "?q=backup")
	if b := basisOf(t, facts, "backup runs"); b != "observed" {
		t.Fatalf("evidenced agent write read as %q, want observed", b)
	}
	for _, f := range facts {
		if strings.Contains(f["text"].(string), "backup runs") {
			ev, _ := f["evidence"].([]any)
			if len(ev) != 2 || ev[0] != "memory/cron.md" {
				t.Fatalf("evidence not returned as a list: %v", f["evidence"])
			}
		}
	}
}

func TestEvidenceDoesNotChangeTheFactId(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	plain := remember(t, h, map[string]any{"topic": "a", "infer": false, "text": "the cache is redis"})
	ev := remember(t, h, map[string]any{"topic": "b", "infer": false, "text": "the cache is redis",
		"evidence": []string{"memory/x.md"}})
	if plain["id"] != ev["id"] {
		t.Fatalf("evidence changed the id (%v vs %v); the id hashes stamp, agent and text only", plain["id"], ev["id"])
	}
}

func TestRememberRefusesMalformedEvidence(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	for _, bad := range [][]string{{"a,b"}, {"line\nbreak"}} {
		w := do(t, h, "POST", "/api/memory", map[string]any{
			"topic": "ops", "text": "a fact", "infer": false, "evidence": bad})
		if w.Code != http.StatusBadRequest {
			t.Errorf("evidence %q = %d, want 400", bad, w.Code)
		}
	}
}

func TestHumanAndImportedAndPulledFactsKeepTheirBasis(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "human": true,
		"text": "the owner is Jeremiah"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "origin": "web:example.com",
		"text": "the vendor ships on fridays"})

	facts := recallFacts(t, h, "?q=owner")
	if b := basisOf(t, facts, "owner is"); b != "stated" {
		t.Fatalf("a person's fact read as %q, want stated", b)
	}
	facts = recallFacts(t, h, "?q=vendor")
	if b := basisOf(t, facts, "vendor ships"); b != "pulled" {
		t.Fatalf("a web fact read as %q, want pulled", b)
	}
}

func TestRecallBasisFilterKeepsOnlyTheListedBases(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "human": true,
		"text": "the team ships on tuesdays"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"evidence": []string{"memory/r.md"}, "text": "the team ships on wednesdays in logs"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the team ships on thursdays, probably"})

	stated := recallFacts(t, h, "?q=team&basis=stated")
	if len(stated) != 1 || basisOf(t, stated, "tuesdays") != "stated" {
		t.Fatalf("basis=stated = %v, want only the person's fact", stated)
	}
	two := recallFacts(t, h, "?q=team&basis=observed,inferred")
	for _, f := range two {
		if f["basis"] == "stated" {
			t.Fatalf("basis=observed,inferred returned a stated fact: %v", f)
		}
	}
	if len(two) != 2 {
		t.Fatalf("basis=observed,inferred returned %d facts, want 2", len(two))
	}
}

func TestRecallRejectsAnUnknownBasisOrMode(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	for _, path := range []string{"/api/memory?q=x&basis=bogus", "/api/memory?q=x&mode=facts",
		"/api/memory/context?q=x&recall_mode=facts"} {
		if w := do(t, h, "GET", path, nil); w.Code != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, w.Code)
		}
	}
}

func TestDefaultRecallIsTheSameAsModeAll(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "category": "preference",
		"text": "the user prefers tabs"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the user deploys to prod-1"})

	plain := do(t, h, "GET", "/api/memory?q=user", nil).Body.Bytes()
	all := do(t, h, "GET", "/api/memory?q=user&mode=all", nil).Body.Bytes()
	if !bytes.Equal(plain, all) {
		t.Fatalf("mode=all is not the default:\n%s\n---\n%s", plain, all)
	}
}

func TestFactualModeDropsPreferencesAndKeepsFacts(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "category": "preference",
		"text": "the user prefers tabs"})
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "category": "likes",
		"text": "the user likes terse answers"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "category": "persona",
		"text": "the user wants a pirate voice"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "category": "fact",
		"text": "the user deploys to prod-1"})

	factual := recallFacts(t, h, "?q=user&mode=factual")
	if len(factual) != 1 || !strings.Contains(factual[0]["text"].(string), "prod-1") {
		t.Fatalf("mode=factual = %v, want only the deploy fact", factual)
	}

	personal := recallFacts(t, h, "?q=user&mode=personal")
	if len(personal) != 3 {
		t.Fatalf("mode=personal returned %d facts, want the three personal ones: %v", len(personal), personal)
	}
	for _, f := range personal {
		if strings.Contains(f["text"].(string), "prod-1") {
			t.Fatalf("mode=personal returned a factual fact: %v", f)
		}
	}
}

func TestPersonalCategoriesCanBeReconfigured(t *testing.T) {
	t.Setenv("GRIMOIRE_PERSONAL_CATEGORIES", "fact")
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "category": "fact",
		"text": "the user runs prod-1"})
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "category": "preference",
		"text": "the user prefers tabs"})

	factual := recallFacts(t, h, "?q=user&mode=factual")
	if len(factual) != 1 || !strings.Contains(factual[0]["text"].(string), "tabs") {
		t.Fatalf("with fact as the personal set, factual should keep preferences: %v", factual)
	}
}

func TestProfileExcludePersonalLeavesPreferencesOut(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "human": true,
		"category": "preference", "text": "the user prefers tabs"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "category": "fact",
		"text": "the deploy host is prod-1"})

	with := do(t, h, "GET", "/api/memory/profile", nil)
	if !strings.Contains(with.Body.String(), "prefers tabs") {
		t.Fatalf("default profile lost the preference:\n%s", with.Body)
	}
	without := do(t, h, "GET", "/api/memory/profile?exclude_personal=true", nil)
	if strings.Contains(without.Body.String(), "prefers tabs") {
		t.Fatalf("exclude_personal=true still lists the preference:\n%s", without.Body)
	}
	if !strings.Contains(without.Body.String(), "prod-1") {
		t.Fatalf("exclude_personal=true dropped a factual fact:\n%s", without.Body)
	}
}

func TestProfileLinesCarryTheBasisTag(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "human": true,
		"text": "the owner is Jeremiah"})
	var doc struct {
		Markdown string `json:"markdown"`
	}
	decode(t, do(t, h, "GET", "/api/memory/profile", nil), &doc)
	if !strings.Contains(doc.Markdown, "- [stated] the owner is Jeremiah") {
		t.Fatalf("profile line has no basis tag:\n%s", doc.Markdown)
	}
}

func TestContextRecallModeFactualLeavesPreferencesOut(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "prefs", "infer": false, "category": "preference",
		"text": "the reviewer prefers tabs over spaces"})
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "category": "fact",
		"text": "the reviewer deploys the gateway to prod-1"})

	ctx := func(query string) string {
		w := do(t, h, "GET", "/api/memory/context?rank=lexical&scope=all&q="+query, nil)
		var out struct {
			Context string `json:"context"`
		}
		decode(t, w, &out)
		return out.Context
	}
	// The contrast only means something if the default surfaces the preference.
	if def := ctx("reviewer"); !strings.Contains(def, "tabs") {
		t.Fatalf("default context did not surface the preference, so the test proves nothing:\n%s", def)
	}
	factual := do(t, h, "GET",
		"/api/memory/context?rank=lexical&scope=all&recall_mode=factual&q=reviewer", nil).Body.String()
	if strings.Contains(factual, "tabs") {
		t.Fatalf("recall_mode=factual context still carries a preference:\n%s", factual)
	}
	if !strings.Contains(factual, "prod-1") {
		t.Fatalf("recall_mode=factual context dropped the factual fact:\n%s", factual)
	}
}

func TestExplainKeepsBasisBesideTheScores(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false, "human": true,
		"text": "the deploy host is prod-1"})
	w := do(t, h, "GET", "/api/memory?q=deploy&explain=1", nil)
	facts := []map[string]any{}
	decode(t, w, &facts)
	if len(facts) == 0 || facts[0]["basis"] != "stated" || facts[0]["scores"] == nil {
		t.Fatalf("explain did not carry basis and scores together: %v", facts)
	}
}
