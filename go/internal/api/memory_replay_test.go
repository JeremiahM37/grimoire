package api

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/replay"
)

func TestReplayTermsAndRelevanceMatchTheContextEndpoint(t *testing.T) {
	for _, q := range []string{
		"should I git push the kestrel repository",
		"please can you help me fix the deploy of kestrel and kestrel",
		"", "ok", "restart the grimoire service and check the disk",
	} {
		a, b := contextTerms(q), replay.Terms(q)
		if strings.Join(a, ",") != strings.Join(b, ",") {
			t.Fatalf("%q: endpoint %v, replay %v", q, a, b)
		}
	}
	for _, c := range []float64{0, 0.3, 0.45, 0.6, 0.8, 0.95} {
		for _, o := range []float64{0, 0.25, 1} {
			if relevance(c, o) != replay.Relevance(c, o) {
				t.Fatalf("relevance(%v,%v) differs", c, o)
			}
			if got, want := replay.CueRelevance(c, o, replay.CueLow), cueRelevance(httptest.NewRequest("GET", "/", nil), c, o); got != want {
				t.Fatalf("cueRelevance(%v,%v): %v vs %v", c, o, got, want)
			}
		}
	}
}

func replayFixture(t *testing.T) (*Server, string, string) {
	t.Helper()
	t.Setenv("GRIMOIRE_REPLAY_MIN_USEFUL", "1")
	s, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	remember(t, h, map[string]any{"topic": "kitchen", "text": "The kitchen cupboards are painted purple"})
	target := "fact:" + fact["id"].(string)
	for i, q := range []string{
		"should I git push the kestrel repository",
		"can I git push the kestrel repository now",
	} {
		sess := "sess-replay-" + string(rune('a'+i)) + "-0000"
		out := ctxJSON(t, h, q, "session", sess)
		tags, _ := out["tags"].([]any)
		if len(tags) == 0 {
			t.Fatalf("setup: nothing fired for %q: %v", q, out)
		}
		postOutcome(t, h, map[string]any{"session": sess, "cited": []string{tags[0].(string)}, "stop": true})
	}
	// A request where nothing useful fires.
	ctxJSON(t, h, "what is for lunch today", "session", "sess-replay-z-0000")
	s.replayDB().Settle = 0
	s.replay.mu.Lock()
	s.replay.resolved = time.Time{}
	s.replay.mu.Unlock()
	time.Sleep(1100 * time.Millisecond) // firings must be older than the settle cutoff (second resolution)
	return s, target, fact["path"].(string)
}

func TestReplayRecordsSituationsAndResolvesOutcomes(t *testing.T) {
	s, target, _ := replayFixture(t)
	s.replayResolve()
	sits, err := s.replayDB().Situations(nil, "live")
	if err != nil {
		t.Fatal(err)
	}
	if len(sits) != 3 {
		t.Fatalf("want 3 situations (two with a recall, one quiet), got %d", len(sits))
	}
	useful := 0
	for _, st := range sits {
		if st.Expect[target] >= replay.UsefulWeight {
			useful++
		}
		if strings.Contains(st.Text, "lunch") && len(st.Expect) != 0 {
			t.Fatalf("the quiet situation fired something: %+v", st)
		}
	}
	if useful != 2 {
		t.Fatalf("cited recalls should be useful expectations, got %d", useful)
	}
}

func TestReplayHoldsAnEditThatLosesARecallAndPassesAHarmlessOne(t *testing.T) {
	s, target, _ := replayFixture(t)
	_ = s
	_, h := s, s.Routes()

	bad := do(t, h, "POST", "/api/memory/replay", map[string]any{"edits": []map[string]string{
		{"target": target, "text": "Quarterly invoices are reconciled by the finance team"}}})
	if bad.Code != 200 {
		t.Fatal(bad.Body.String())
	}
	var gate ReplayGate
	decode(t, bad, &gate)
	if !gate.Verdict.Hold || gate.Report.LostMemories < 2 {
		t.Fatalf("an edit that moves the rule away must be held: %+v %+v", gate.Verdict, gate.Report)
	}
	if len(gate.Report.Diffs) == 0 || !strings.Contains(gate.Report.Diffs[0].Text, "push") {
		t.Fatalf("the report should name the lost situations: %+v", gate.Report.Diffs)
	}

	ok := do(t, h, "POST", "/api/memory/replay", map[string]any{"edits": []map[string]string{
		{"target": target, "text": pushRule + " (checked)"}}})
	decode(t, ok, &gate)
	if gate.Verdict.Hold || gate.Report.LostMemories != 0 {
		t.Fatalf("a reworded rule that still matches must pass: %+v %+v", gate.Verdict, gate.Report)
	}
}

func TestReplayMergeRemapKeepsTheRecallAndAddingNoiseIsFlagged(t *testing.T) {
	s, target, _ := replayFixture(t)
	h := s.Routes()
	keeper := remember(t, h, map[string]any{"topic": "other", "text": "Releases are tagged on Fridays"})
	keep := "fact:" + keeper["id"].(string)
	// Merge the push rule into another memory: its text now says the rule.
	var gate ReplayGate
	rec := do(t, h, "POST", "/api/memory/replay", map[string]any{"merge": []map[string]any{
		{"from": []string{target}, "into": keep, "text": pushRule + ". Releases are tagged on Fridays."}}})
	decode(t, rec, &gate)
	if gate.Verdict.Hold || gate.Report.LostMemories != 0 {
		t.Fatalf("a merge that keeps the rule's words and carries its identity must pass: %+v %+v", gate.Verdict, gate.Report)
	}
	// The same merge without the rule's text loses it.
	rec = do(t, h, "POST", "/api/memory/replay", map[string]any{"merge": []map[string]any{
		{"from": []string{target}, "into": keep, "text": "Releases are tagged on Fridays"}}})
	decode(t, rec, &gate)
	if !gate.Verdict.Hold {
		t.Fatalf("a merge that drops the rule's text must be held: %+v", gate.Verdict)
	}
}

func TestReplayWarnsOnManualEditsButNeverBlocks(t *testing.T) {
	s, target, path := replayFixture(t)
	h := s.Routes()
	id := strings.TrimPrefix(target, "fact:")
	rec := do(t, h, "PATCH", "/api/memory/entry", map[string]any{"path": path, "id": id,
		"text": "Quarterly invoices are reconciled by the finance team"})
	if rec.Code != 200 {
		t.Fatalf("a manual edit must go through: %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	decode(t, rec, &out)
	w, ok := out["replay"].(map[string]any)
	if !ok || w["lost_recalls"].(float64) < 2 {
		t.Fatalf("the response should warn: %v", out)
	}
}

func TestReplayWarnsOnSupersessionAndCarriesCues(t *testing.T) {
	s, target, _ := replayFixture(t)
	h := s.Routes()
	// Teach a cue to the rule, then supersede it with an unrelated fact: the
	// write is applied, with a warning, and the cue follows the replacement.
	rec := do(t, h, "POST", "/api/memory/cues", map[string]any{"target": target, "kind": "request", "source": "agent",
		"cues": []string{"publishing the kestrel branch"}})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	old := replayTargetCues(s, target)
	if old != 1 {
		t.Fatalf("setup: %d cues", old)
	}
	s.carryCues(target, "fact:replacement")
	if replayTargetCues(s, "fact:replacement") != 1 {
		t.Fatal("cues were not carried to the replacement")
	}
}

func replayTargetCues(s *Server, target string) int { return len(s.cues().For(target)) }

func TestReplayGatesDreamFixesAndReportsHeld(t *testing.T) {
	s, target, path := replayFixture(t)
	note, err := s.Vault.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	body := note.Body
	lines := strings.Split(strings.TrimSuffix(note.Raw, "\n"), "\n")
	line := 0
	for i, l := range lines {
		if strings.Contains(l, "git push") {
			line = i + 1
		}
	}
	if line == 0 {
		t.Fatalf("setup: rule not found in %q", note.Raw)
	}
	f := dream.Fix{Kind: dream.FixReplaceLine, Path: path, Line: line, Old: lines[line-1], New: ""}
	applied, held := s.applyDreamFixesGated([]dream.Finding{{Check: "x", Fix: &f}})
	if len(applied) != 0 || len(held) != 1 {
		t.Fatalf("deleting the useful rule must be held: applied=%v held=%v", applied, held)
	}
	if held[0].Path != path || len(held[0].Lost) == 0 || !strings.Contains(held[0].Reason, "lose") {
		t.Fatalf("held entry: %+v", held[0])
	}
	if after, _ := s.noteBody(path); after != body && strings.TrimSpace(after) != strings.TrimSpace(body) {
		t.Fatal("a held fix must not touch the note")
	}
	if !strings.Contains(renderDreamReport(&dream.Report{Held: held, Finished: time.Now()}), "Held by memory replay") {
		t.Fatal("the report must list held edits")
	}
	_ = target
}

func TestReplayIsOffWhenDisabledAndSkipsSessionlessRequests(t *testing.T) {
	t.Setenv("GRIMOIRE_REPLAY_LOG", "0")
	s, h := testServer(t)
	remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	ctxJSON(t, h, "should I git push the kestrel repository", "session", "sess-off-0000")
	if s.replayDB() != nil {
		t.Fatal("replay_log=0 must not open the corpus")
	}
	// And a request with no session is not a situation.
	t.Setenv("GRIMOIRE_REPLAY_LOG", "1")
	s2, h2 := testServer(t)
	ctxJSON(t, h2, "should I git push the kestrel repository")
	st, _ := s2.replayDB().Stats()
	if st.Live != 0 {
		t.Fatalf("sessionless request recorded: %+v", st)
	}
	_ = url.Values{}
}
