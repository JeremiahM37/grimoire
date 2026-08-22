package api

import (
	"net/http"
	"testing"
)

// A refused supersession has to leave something a person can act on.
//
// Before challenges, the two contradictory facts sat in the note with nothing
// joining them — which is exactly what the immutable flag already produced, and
// it reads as clutter rather than as a question.

// contested writes a fact a person stands behind, then has an agent contradict
// it, and returns the note they live in.
func contested(t *testing.T, h http.Handler) string {
	t.Helper()
	remember(t, h, map[string]any{
		"text": "Billing Postgres runs on port 6432", "topic": "ops",
		"agent": "me", "human": true,
	})
	out := remember(t, h, map[string]any{
		"text": "Billing Postgres runs on port 5432", "topic": "ops",
		"agent": "claude",
	})
	results, _ := out["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("no results from the contradicting write: %v", out)
	}
	first, _ := results[0].(map[string]any)
	if first["op"] != "ADD" {
		t.Fatalf("op = %v, want ADD — the agent must not supersede a person's "+
			"fact, which is the whole rule", first["op"])
	}
	if first["challenges"] == nil || first["challenges"] == "" {
		t.Fatal("the write was refused but recorded no challenge, so nothing " +
			"downstream can tell a disagreement from an unrelated second fact")
	}
	path, _ := first["path"].(string)
	return path
}

func openChallenges(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	w := do(t, h, "GET", "/api/memory/challenges", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("challenges = %d: %s", w.Code, w.Body)
	}
	var out []map[string]any
	decode(t, w, &out)
	return out
}

func TestARefusedSupersessionOpensAChallenge(t *testing.T) {
	_, h := testServer(t)
	contested(t, h)

	open := openChallenges(t, h)
	if len(open) != 1 {
		t.Fatalf("open challenges = %d, want 1 — a refusal nobody can see is a "+
			"refusal nobody can act on", len(open))
	}
	if open[0]["contested_authority"] != "human" {
		t.Errorf("contested_authority = %v, want human", open[0]["contested_authority"])
	}
	if open[0]["contested_text"] != "Billing Postgres runs on port 6432" {
		t.Errorf("contested_text = %v", open[0]["contested_text"])
	}
}

// Upholding retracts the agent's claim and leaves the person's standing.
func TestUpholdingAChallengeRetractsTheAgentsClaim(t *testing.T) {
	_, h := testServer(t)
	note := contested(t, h)
	open := openChallenges(t, h)

	w := do(t, h, "POST", "/api/memory/challenge", map[string]any{
		"note": note, "id": open[0]["id"], "resolution": "uphold"})
	if w.Code != http.StatusOK {
		t.Fatalf("uphold = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["stands"] != open[0]["contested_id"] {
		t.Fatalf("stands = %v, want the person's fact %v",
			res["stands"], open[0]["contested_id"])
	}
	if got := openChallenges(t, h); len(got) != 0 {
		t.Errorf("open challenges after settling = %d, want 0 — a settled "+
			"challenge must stop asking", len(got))
	}
}

// Conceding is how a person corrects THEMSELVES. Without it the lattice would
// make the operator's first answer permanent, which is a worse failure than the
// one it exists to fix.
func TestConcedingLetsTheAgentsValueWin(t *testing.T) {
	_, h := testServer(t)
	note := contested(t, h)
	open := openChallenges(t, h)

	w := do(t, h, "POST", "/api/memory/challenge", map[string]any{
		"note": note, "id": open[0]["id"], "resolution": "concede"})
	if w.Code != http.StatusOK {
		t.Fatalf("concede = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["stands"] != open[0]["id"] {
		t.Fatalf("stands = %v, want the agent's claim %v", res["stands"], open[0]["id"])
	}
	if res["superseded"] != open[0]["contested_id"] {
		t.Fatalf("superseded = %v, want the person's earlier fact", res["superseded"])
	}
	// The value the person accepted is now one they stand behind, so a later
	// agent write must not be able to revert it either.
	facts := recallFacts(t, h, "?q=postgres")
	for _, f := range facts {
		if f["id"] == open[0]["id"] && f["authority"] != "human" {
			t.Errorf("authority = %v after conceding, want human: accepting a "+
				"value is asserting it", f["authority"])
		}
	}
}

func TestResolutionMustBeUpholdOrConcede(t *testing.T) {
	_, h := testServer(t)
	note := contested(t, h)
	open := openChallenges(t, h)
	w := do(t, h, "POST", "/api/memory/challenge", map[string]any{
		"note": note, "id": open[0]["id"], "resolution": "maybe"})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// A challenge against a fact that is no longer standing is not a question any
// more, and asking a person to rule on it would be asking about nothing.
func TestAChallengeAgainstARetractedFactIsNotListed(t *testing.T) {
	_, h := testServer(t)
	note := contested(t, h)
	open := openChallenges(t, h)

	w := do(t, h, "DELETE", "/api/memory/entry?path="+note+"&id="+
		open[0]["contested_id"].(string), nil)
	if w.Code >= 400 {
		t.Fatalf("forget = %d: %s", w.Code, w.Body)
	}
	if got := openChallenges(t, h); len(got) != 0 {
		t.Errorf("open challenges = %d, want 0 once the contested fact is gone",
			len(got))
	}
}
