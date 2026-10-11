package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The lattice applies to a plain forget: a caller below an entry's rung may
// not forget it, soft or hard, and nothing is written. A person may forget
// anything, and an agent may forget a fact on its own rung.

const (
	rungPersonFact = "Analytics Postgres runs on port 6543"
	rungAgentFact  = "Billing Postgres runs on port 5432"
	rungNote       = "memory/ops.md"
)

// rungFixture writes one person's fact and one agent's fact, in different
// slots, and returns the ids of both.
func rungFixture(t *testing.T, h http.Handler) (personID, agentID string) {
	t.Helper()
	out := remember(t, h, map[string]any{"text": rungPersonFact, "topic": "ops", "agent": "me", "human": true})
	results, _ := out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("person write results = %v", out)
	}
	personID = results[0].(map[string]any)["id"].(string)
	out = remember(t, h, map[string]any{"text": rungAgentFact, "topic": "ops", "agent": "claude"})
	results, _ = out["results"].([]any)
	if len(results) != 1 {
		t.Fatalf("agent write results = %v", out)
	}
	agentID = results[0].(map[string]any)["id"].(string)
	return personID, agentID
}

// forgetWith posts a plain forget. agent and origin go in the body; header
// names an agent on the request only, with no body field.
func forgetWith(t *testing.T, h http.Handler, id string, hard bool, agent, origin, header string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"path": rungNote, "id": id, "hard": hard}
	if agent != "" {
		body["agent"] = agent
	}
	if origin != "" {
		body["origin"] = origin
	}
	req := requestFor(t, "POST", "/api/memory/forget", body)
	if header != "" {
		req.Header.Set("X-Grimoire-Agent", header)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestAnOutrankedCallerCannotForgetAPersonsFact(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	personID, _ := rungFixture(t, h)

	for _, c := range []struct {
		name   string
		agent  string
		header string
		hard   bool
	}{
		{"agent in body, soft", "claude", "", false},
		{"agent in body, hard", "claude", "", true},
		{"agent in header only, soft", "", "claude", false},
		{"agent in header only, hard", "", "claude", true},
	} {
		w := forgetWith(t, h, personID, c.hard, c.agent, "", c.header)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s = %d, want 403: %s", c.name, w.Code, w.Body)
		}
	}
	// Nothing was written: the person's fact is still believed at human rung.
	facts := recallFacts(t, h, "?q=analytics+postgres")
	found := false
	for _, f := range facts {
		if f["text"] == rungPersonFact {
			found = true
			if f["authority"] != "human" {
				t.Errorf("person's fact authority = %v after refused forgets", f["authority"])
			}
		}
	}
	if !found {
		t.Fatalf("the person's fact is no longer believed after refused forgets: %v", facts)
	}
}

func TestAPulledCallerCannotForgetAnAgentsFact(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	_, agentID := rungFixture(t, h)

	w := forgetWith(t, h, agentID, false, "jira", "connector:jira:OPS-1", "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("pulled forget of an agent's fact = %d, want 403: %s", w.Code, w.Body)
	}
}

func TestAllowedForgetsStillWork(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	personID, agentID := rungFixture(t, h)

	// A person may forget a fact at any rung, including her own.
	if w := forgetWith(t, h, personID, false, "", "", ""); w.Code != http.StatusOK {
		t.Fatalf("person soft forget = %d: %s", w.Code, w.Body)
	}
	// An agent may forget a fact on its own rung, and the record names it.
	if w := forgetWith(t, h, agentID, false, "claude", "", ""); w.Code != http.StatusOK {
		t.Fatalf("agent forget of its own fact = %d: %s", w.Code, w.Body)
	}
}
