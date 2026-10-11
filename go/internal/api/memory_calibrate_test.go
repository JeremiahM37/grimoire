package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Calibration through the real write paths. Each test drives the same routes a
// client would, then reads the reliability endpoint, so a hook that stopped
// firing shows up here rather than only in the pure package.

// pulledContradiction has an agent write a fact, then a PULLED document
// contradict it. Pulled text may not overwrite an agent's fact, so the write
// is recorded as a challenge against the agent's fact.
func pulledContradiction(t *testing.T, h http.Handler, agent, topic, first, second string) string {
	t.Helper()
	remember(t, h, map[string]any{"text": first, "topic": topic, "agent": agent})
	out := remember(t, h, map[string]any{
		"text": second, "topic": topic, "agent": "web-reader",
		"origin": "https://example.com/post",
	})
	results, _ := out["results"].([]any)
	if len(results) == 0 {
		t.Fatalf("no result for %q: %v", second, out)
	}
	first0, _ := results[0].(map[string]any)
	if c, _ := first0["challenges"].(string); c == "" {
		t.Fatalf("%q did not challenge %q (op=%v); the fixture is not a contradiction", second, first, first0["op"])
	}
	path, _ := first0["path"].(string)
	return path
}

func reliabilityRows(t *testing.T, h http.Handler) map[string]map[string]any {
	t.Helper()
	w := do(t, h, "GET", "/api/memory/agents/reliability", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("reliability = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Agents []map[string]any `json:"agents"`
	}
	decode(t, w, &out)
	rows := map[string]map[string]any{}
	for _, a := range out.Agents {
		name, _ := a["agent"].(string)
		rows[name] = a
	}
	return rows
}

// The explicit route check: a challenged agent's fact lands as a challenge on
// that agent, and a settled challenge credits the winner and debits the loser.
func TestChallengedFactsLowerTheirWriterOnTheReliabilityEndpoint(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	for i, pair := range [][2]string{
		{"Billing cache TTL is 300 seconds", "Billing cache TTL is 60 seconds"},
		{"Session timeout is 30 minutes", "Session timeout is 90 minutes"},
		{"Ingest workers run 4 threads", "Ingest workers run 8 threads"},
	} {
		topic := []string{"ops", "sessions", "ingest"}[i]
		pulledContradiction(t, h, "sloppy", topic, pair[0], pair[1])
	}

	rows := reliabilityRows(t, h)
	sloppy, ok := rows["sloppy"]
	if !ok {
		t.Fatalf("sloppy has no row; rows = %v", rows)
	}
	if got := sloppy["challenged"]; got != float64(3) {
		t.Fatalf("challenged = %v, want 3 (one per contradicted fact)", got)
	}
	if got := sloppy["reliability"].(float64); got >= 0.5 {
		t.Fatalf("reliability = %.3f after three challenges and no confirmations, want below the prior mean", got)
	}
}

// Settling a challenge is a person's ruling, and it is scored like one. When
// the person concedes, the challenger was right: its writer is confirmed and
// the agent whose fact lost is superseded by a human.
func TestConcedingScoresTheChallengerAndTheLoser(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	note := pulledContradiction(t, h, "sloppy", "ops",
		"Billing cache TTL is 300 seconds", "Billing cache TTL is 60 seconds")
	open := openChallenges(t, h)
	if len(open) != 1 {
		t.Fatalf("open challenges = %d, want 1", len(open))
	}
	w := do(t, h, "POST", "/api/memory/challenge", map[string]any{
		"note": note, "id": open[0]["id"], "resolution": "concede"})
	if w.Code != http.StatusOK {
		t.Fatalf("concede = %d: %s", w.Code, w.Body)
	}
	rows := reliabilityRows(t, h)
	if r := rows["sloppy"]; r == nil || r["superseded_by_human"] != float64(1) {
		t.Errorf("sloppy after conceding = %v, want superseded_by_human 1", r)
	}
	if r := rows["web-reader"]; r == nil || r["confirmed"] != float64(1) {
		t.Errorf("web-reader after conceding = %v, want confirmed 1", r)
	}
}

// Confirmation needs an independent writer: the same agent re-checking its own
// fact is not evidence about it, but a different agent confirming it is.
func TestConfirmationNeedsAnIndependentWriter(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	const text = "Staging DB is on port 5440"
	path, id := rememberAs(t, h, "", "ops", text, "solo", false)
	recheck := func(agent string) {
		t.Helper()
		w := do(t, h, "POST", "/api/memory", map[string]any{
			"text": text, "topic": "ops", "agent": agent, "target_id": id,
			"target_path": path, "expected_text": text,
		})
		if w.Code >= 400 {
			t.Fatalf("recheck by %s = %d: %s", agent, w.Code, w.Body)
		}
	}
	recheck("solo")
	if row, ok := reliabilityRows(t, h)["solo"]; ok && row["confirmed"] != float64(0) {
		t.Fatalf("self re-check scored as confirmed: %v", row["confirmed"])
	}
	recheck("auditor")
	if row := reliabilityRows(t, h)["solo"]; row == nil || row["confirmed"] != float64(1) {
		t.Fatalf("independent re-check did not confirm the writer: %v", row)
	}
}

// Explain carries the writer's reliability for agent-written facts, and the
// plain recall does not, so the default response shape is unchanged.
func TestExplainCarriesWriterReliability(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	pulledContradiction(t, h, "sloppy", "ops", "Billing cache TTL is 300 seconds", "Billing cache TTL is 60 seconds")
	remember(t, h, map[string]any{"text": "Deploy window is Tuesday", "topic": "ops", "agent": "careful"})

	plain := recallFacts(t, h, "?q=deploy+window")
	for _, f := range plain {
		if _, has := f["agent_reliability"]; has {
			t.Fatal("agent_reliability leaked into a non-explain recall")
		}
	}
	explained := recallFacts(t, h, "?q=deploy+window&explain=true")
	var found bool
	for _, f := range explained {
		if f["agent"] == "careful" {
			found = true
			if _, ok := f["agent_reliability"].(float64); !ok {
				t.Fatalf("explain for an agent-written fact has no agent_reliability: %v", f)
			}
		}
	}
	if !found {
		t.Fatalf("explained recall did not return the agent's fact: %v", explained)
	}
}

// Recall counts are the input to eviction ordering. Recalling a fact must
// count it, and a fact nobody recalled must not count.
func TestRecallCountsFeedEvictionOrdering(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	remember(t, h, map[string]any{"text": "Mail relay is on port 587", "topic": "ops", "agent": "a"})
	remember(t, h, map[string]any{"text": "Mail relay is a dinosaur", "topic": "ops", "agent": "a"})

	// Count what recall actually returned, so the expectation is observed
	// rather than assumed about the ranker.
	returned := map[string]int{}
	var notePath string
	for i := 0; i < 2; i++ {
		for _, f := range recallFacts(t, h, "?q=mail+relay+port") {
			id, _ := f["id"].(string)
			returned[id]++
			notePath, _ = f["path"].(string)
		}
	}
	if len(returned) == 0 || notePath == "" {
		t.Fatal("recall returned nothing; the premise of this test does not hold")
	}
	n, err := s.Vault.Read(notePath)
	if err != nil {
		t.Fatal(err)
	}
	st, err := s.calibrationStore()
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, e := range memory.Parse(n.Body) {
		got, err := st.Recalled(notePath, e.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got != returned[e.ID] {
			t.Fatalf("recalled count for %q = %d, recall returned it %d times", e.Text, got, returned[e.ID])
		}
		if returned[e.ID] > 0 {
			seen++
		}
	}
	if seen == 0 {
		t.Fatal("no fact was counted as recalled")
	}
}

// Eviction proposals: an old fact nobody recalled is proposed before an old
// fact that has been recalled, and a frequently used fact is never proposed.
func TestPruneOrderingPrefersTheUnused(t *testing.T) {
	t.Parallel()
	s, _ := testServer(t)
	old := vault.Now().Add(-400 * 24 * time.Hour).Format(memory.StampFormat)
	hits := []index.MemoryHit{
		{Entry: memory.Entry{ID: "recalled", Agent: "x", Stamp: old, Importance: 2}, Note: "memory/a.md"},
		{Entry: memory.Entry{ID: "untouched", Agent: "x", Stamp: old, Importance: 2}, Note: "memory/a.md"},
		{Entry: memory.Entry{ID: "used", Agent: "x", Stamp: old, Importance: 2}, Note: "memory/a.md", Uses: 3},
	}
	st, err := s.calibrationStore()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.NoteRecalled("memory/a.md", "recalled"); err != nil {
		t.Fatal(err)
	}
	got := s.orderPruneCandidates(hits, vault.Now())
	if len(got) != 2 {
		t.Fatalf("proposals = %d, want 2 (the frequently used fact is protected)", len(got))
	}
	if got[0].ID != "untouched" || got[1].ID != "recalled" {
		t.Fatalf("order = [%s %s], want [untouched recalled]", got[0].ID, got[1].ID)
	}
}
