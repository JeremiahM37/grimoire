package api

import (
	"testing"
)

// The disputes listing carries a suggested winner and the score behind it. A
// suggestion that resolved anything, or that could outrank a person's fact,
// would break the rule that settling a dispute is a person's act.

func suggestionOf(t *testing.T, d map[string]any) (id, side string) {
	t.Helper()
	s, ok := d["suggestion"].(map[string]any)
	if !ok {
		t.Fatalf("listing has no suggestion: %v", d)
	}
	id, _ = s["id"].(string)
	side, _ = s["side"].(string)
	return id, side
}

func TestDisputeListingSuggestsThePersonsFactAndShowsWhy(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)

	open := disputes(t, h)
	if len(open) != 1 {
		t.Fatalf("disputes = %d, want 1", len(open))
	}
	d := open[0]
	id, side := suggestionOf(t, d)
	if side != "disputed" || id != d["id"] {
		t.Fatalf("suggestion = %s/%s, want the person's disputed fact %v", id, side, d["id"])
	}

	rows, _ := d["breakdown"].([]any)
	if len(rows) != 2 {
		t.Fatalf("breakdown has %d sides, want 2: %v", len(rows), rows)
	}
	first := rows[0].(map[string]any)
	if first["side"] != "disputed" {
		t.Fatalf("breakdown is not best first: %v", rows)
	}
	for _, k := range []string{"authority", "verification", "corroboration", "recency", "reliability", "total"} {
		if _, ok := first[k].(float64); !ok {
			t.Fatalf("breakdown row carries no %q term: %v", k, first)
		}
	}
}

// Suggesting is not settling: the dispute is still open after the listing has
// been read, and nothing has been written.
func TestDisputeSuggestionResolvesNothing(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	before := changesSince(t, h)
	disputes(t, h)
	disputes(t, h)
	if len(disputes(t, h)) != 1 {
		t.Fatal("reading the suggestion closed the dispute")
	}
	if len(changesSince(t, h)) != len(before) {
		t.Fatal("reading the suggestion wrote a belief change")
	}
}

// An agent challenger with verified evidence still does not outrank the
// person's fact: the listing favours the person whatever the challenger cites.
func TestVerifiedAgentEvidenceDoesNotOutrankAPersonsFact(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	if _, err := s.Vault.Write("ops/runbook.md", "Billing Postgres runs on port 5432\n", nil); err != nil {
		t.Fatal(err)
	}
	remember(t, h, map[string]any{"text": "Billing Postgres runs on port 6432", "topic": "ops",
		"agent": "me", "human": true})
	remember(t, h, map[string]any{"text": "Billing Postgres runs on port 5432", "topic": "ops",
		"agent": "claude", "evidence": []string{"ops/runbook.md"}})

	open := disputes(t, h)
	if len(open) != 1 {
		t.Fatalf("disputes = %d, want 1", len(open))
	}
	if _, side := suggestionOf(t, open[0]); side != "disputed" {
		t.Fatalf("a verified agent challenger outranked a person's fact (suggested %s)", side)
	}
}
