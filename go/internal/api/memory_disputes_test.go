package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A dispute is a fact a person recorded, contested by an agent that was not
// allowed to replace it. These tests settle one each way and check the
// outcome on the three surfaces a person reads: the disputes list, the recall
// that follows, and the belief-change digest that records what happened.

func disputes(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	w := do(t, h, "GET", "/api/memory/disputes", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("disputes = %d: %s", w.Code, w.Body)
	}
	var out []map[string]any
	decode(t, w, &out)
	return out
}

func resolve(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	return do(t, h, "POST", "/api/memory/disputes/resolve", body)
}

// changesSince returns the belief-change digest as the rows a person reads.
func changesSince(t *testing.T, h http.Handler) []map[string]any {
	t.Helper()
	w := do(t, h, "GET", "/api/memory/changes?since=1970-01-01T00:00:00Z", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("changes = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Changes []map[string]any `json:"changes"`
	}
	decode(t, w, &out)
	return out.Changes
}

func changeRow(rows []map[string]any, text string) map[string]any {
	for _, r := range rows {
		if r["text"] == text {
			return r
		}
	}
	return nil
}

const (
	humanFact = "Billing Postgres runs on port 6432"
	agentFact = "Billing Postgres runs on port 5432"
)

func TestDisputesListBothSidesWithAuthorityAndEvidence(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)

	open := disputes(t, h)
	if len(open) != 1 {
		t.Fatalf("disputes = %d, want 1", len(open))
	}
	d := open[0]
	disputed, _ := d["disputed"].(map[string]any)
	if disputed["text"] != humanFact || disputed["authority"] != "human" {
		t.Errorf("disputed side = %v, want the person's fact at human authority", disputed)
	}
	if disputed["id"] != d["id"] {
		t.Errorf("top-level id %v does not name the disputed entry %v", d["id"], disputed["id"])
	}
	if _, ok := disputed["stamp"].(string); !ok {
		t.Errorf("disputed side carries no stamp: %v", disputed)
	}
	if _, ok := disputed["evidence"].([]any); !ok {
		t.Errorf("evidence must be an array, even when empty: %v", disputed["evidence"])
	}
	ch, _ := d["challengers"].([]any)
	if len(ch) != 1 {
		t.Fatalf("challengers = %d, want 1", len(ch))
	}
	c := ch[0].(map[string]any)
	if c["text"] != agentFact || c["authority"] != "agent" {
		t.Errorf("challenger = %v, want the agent's claim at agent authority", c)
	}
	if c["stamp"] == nil || c["stamp"] == "" {
		t.Errorf("challenger carries no stamp: %v", c)
	}
}

// An agent caller is refused before anything is looked up, so the refusal is
// the same whether or not the id exists.
func TestAnAgentCannotResolveADispute(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)
	id := open[0]["id"]

	for _, body := range []map[string]any{
		{"id": id, "resolution": "keep"},
		{"id": id, "resolution": "accept_challenger"},
		{"id": id, "resolution": "merge", "text": "x"},
		{"id": "no-such-entry", "resolution": "keep"},
	} {
		req := requestFor(t, "POST", "/api/memory/disputes/resolve", body)
		req.Header.Set("X-Grimoire-Agent", "claude")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("agent resolve %v = %d, want 403: %s", body, w.Code, w.Body)
		}
	}
	if got := disputes(t, h); len(got) != 1 {
		t.Fatalf("disputes after refused agent resolves = %d, want 1 — a refused "+
			"resolution must change nothing", len(got))
	}
}

func TestKeepRetractsTheChallengerAndConfirmsThePerson(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)

	w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "keep"})
	if w.Code != http.StatusOK {
		t.Fatalf("keep = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["stands"] != open[0]["id"] {
		t.Errorf("stands = %v, want the person's fact", res["stands"])
	}
	if got := disputes(t, h); len(got) != 0 {
		t.Errorf("disputes after keep = %d, want 0", len(got))
	}
	rows := changesSince(t, h)
	row := changeRow(rows, agentFact)
	if row == nil || row["kind"] != "retracted" || row["agent"] != "human" {
		t.Errorf("the challenger's retraction is not in memory_changes as a human "+
			"retraction: %v", row)
	}
	facts := recallFacts(t, h, "?q=postgres")
	for _, f := range facts {
		if f["text"] == humanFact && f["authority"] != "human" {
			t.Errorf("confirmed fact authority = %v, want human", f["authority"])
		}
	}
	for _, f := range facts {
		if f["text"] == agentFact {
			t.Errorf("the retracted challenger is still recalled as a belief")
		}
	}
}

func TestAcceptChallengerSupersedesTheOriginal(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)

	w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "accept_challenger"})
	if w.Code != http.StatusOK {
		t.Fatalf("accept = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	if res["stands"] != open[0]["challengers"].([]any)[0].(map[string]any)["id"] {
		t.Errorf("stands = %v, want the challenger", res["stands"])
	}
	if got := disputes(t, h); len(got) != 0 {
		t.Errorf("disputes after accept = %d, want 0", len(got))
	}
	row := changeRow(changesSince(t, h), agentFact)
	if row == nil || row["kind"] != "changed" || row["replaced_text"] != humanFact {
		t.Errorf("accepting the challenger is not a changed row carrying both texts: %v", row)
	}
	for _, f := range recallFacts(t, h, "?q=postgres") {
		if f["text"] == agentFact && f["authority"] != "human" {
			t.Errorf("accepted value authority = %v, want human: accepting asserts it", f["authority"])
		}
		if f["text"] == humanFact {
			t.Errorf("the replaced fact is still recalled")
		}
	}
}

func TestMergeWritesAHumanEntryThatReplacesBoth(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)
	merged := "Billing Postgres runs on port 6432 behind pgbouncer"

	w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "merge", "text": merged})
	if w.Code != http.StatusOK {
		t.Fatalf("merge = %d: %s", w.Code, w.Body)
	}
	var res map[string]any
	decode(t, w, &res)
	entry, _ := res["entry"].(map[string]any)
	if entry["text"] != merged {
		t.Fatalf("merge response entry = %v, want the merged text", entry)
	}
	if got := disputes(t, h); len(got) != 0 {
		t.Errorf("disputes after merge = %d, want 0", len(got))
	}
	// Both replaced beliefs must appear as changed rows, each pointing at the
	// merged text as its successor and carrying the text it replaced.
	rows := changesSince(t, h)
	for _, old := range []string{humanFact, agentFact} {
		var hit map[string]any
		for _, r := range rows {
			if r["kind"] == "changed" && r["text"] == merged && r["replaced_text"] == old {
				hit = r
			}
		}
		if hit == nil {
			t.Errorf("replacing %q with the merge has no changed row in memory_changes", old)
		}
	}
	found := false
	for _, f := range recallFacts(t, h, "?q=pgbouncer") {
		if f["text"] == merged {
			found = true
			if f["authority"] != "human" {
				t.Errorf("merged authority = %v, want human", f["authority"])
			}
		}
	}
	if !found {
		t.Error("the merged fact is not recalled")
	}
}

func TestMergeNeedsText(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)
	for _, text := range []string{"", "   "} {
		w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "merge", "text": text})
		if w.Code != http.StatusBadRequest {
			t.Errorf("merge with text %q = %d, want 400", text, w.Code)
		}
	}
	if got := disputes(t, h); len(got) != 1 {
		t.Errorf("a refused merge settled the dispute")
	}
}

func TestResolveRefusesWhatIsNotDisputed(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	open := disputes(t, h)

	if w := resolve(t, h, map[string]any{"id": "no-such-entry", "resolution": "keep"}); w.Code != http.StatusNotFound {
		t.Errorf("unknown id = %d, want 404", w.Code)
	}
	if w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "maybe"}); w.Code != http.StatusBadRequest {
		t.Errorf("unknown resolution = %d, want 400", w.Code)
	}
	// The challenger is not disputed: nothing contests it.
	if w := resolve(t, h, map[string]any{"id": open[0]["challengers"].([]any)[0].(map[string]any)["id"], "resolution": "keep"}); w.Code != http.StatusConflict {
		t.Errorf("undisputed entry = %d, want 409", w.Code)
	}
	if w := resolve(t, h, map[string]any{"id": open[0]["id"], "resolution": "keep", "challenger": "nope"}); w.Code != http.StatusBadRequest {
		t.Errorf("a challenger that does not contest it = %d, want 400", w.Code)
	}
}

// Two contesting claims: accepting one needs the person to say which.
func TestAcceptNamesTheChallengerWhenSeveralContest(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	contested(t, h)
	remember(t, h, map[string]any{"text": "Billing Postgres runs on port 5433", "topic": "ops", "agent": "codex"})
	open := disputes(t, h)
	if len(open) != 1 {
		t.Fatalf("disputes = %d, want 1 with two challengers", len(open))
	}
	challengers := open[0]["challengers"].([]any)
	if len(challengers) != 2 {
		t.Fatalf("challengers = %d, want 2", len(challengers))
	}
	id := open[0]["id"]

	w := resolve(t, h, map[string]any{"id": id, "resolution": "accept_challenger"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "challenger") {
		t.Fatalf("accept without a challenger = %d: %s", w.Code, w.Body)
	}
	pick := challengers[1].(map[string]any)["id"]
	w = resolve(t, h, map[string]any{"id": id, "resolution": "accept_challenger", "challenger": pick})
	if w.Code != http.StatusOK {
		t.Fatalf("accept named = %d: %s", w.Code, w.Body)
	}
	if got := disputes(t, h); len(got) != 0 {
		t.Errorf("disputes after accept = %d, want 0", len(got))
	}
}
