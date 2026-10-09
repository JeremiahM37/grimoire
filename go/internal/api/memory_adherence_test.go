package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

const pushRule = "Never run git push on the kestrel repository without being asked"

func ctxJSON(t *testing.T, h http.Handler, q string, extra ...string) map[string]any {
	t.Helper()
	v := url.Values{"q": {q}, "rank": {"hybrid"}, "min_rel": {"0.3"}}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	rec := do(t, h, "GET", "/api/memory/context?"+v.Encode(), nil)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var out map[string]any
	decode(t, rec, &out)
	return out
}

func postOutcome(t *testing.T, h http.Handler, body map[string]any) map[string]any {
	t.Helper()
	rec := do(t, h, "POST", "/api/memory/outcome", body)
	if rec.Code != 200 {
		t.Fatalf("outcome %d %s", rec.Code, rec.Body)
	}
	var out map[string]any
	decode(t, rec, &out)
	return out
}

func TestInjectedItemsCarryTagsAndJSONStaysUnchanged(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	out := ctxJSON(t, h, "should I git push the kestrel repository", "session", "sess-aaaa-1111")
	tags := out["tags"].([]any)
	text := out["context"].(string)
	if len(tags) != 1 || !strings.Contains(text, "m:"+tags[0].(string)+" ") || !strings.Contains(text, "cite its tag once") {
		t.Fatalf("no marker: %v", out)
	}
	if !strings.HasPrefix(out["keys"].([]any)[0].(string), tags[0].(string)) {
		t.Fatal("tag is not a key prefix")
	}
	js := ctxJSON(t, h, "should I git push the kestrel repository", "format", "json")
	if _, has := js["tags"]; has || strings.Contains(js["context"].(string), "m:") ||
		!strings.Contains(js["context"].(string), `"key"`) {
		t.Fatalf("format=json changed: %v", js)
	}
}

func TestOutcomeCitedFollowedViolatedIgnored(t *testing.T) {
	s, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	target := "fact:" + fact["id"].(string)
	rec := do(t, h, "POST", "/api/memory/check", map[string]any{"target": target, "forbid": `\bgit\s+push\b`})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	sess := "sess-bbbb-2222"

	// Turn 1: the agent cites the tag.
	out := ctxJSON(t, h, "should I git push the kestrel repository", "session", sess)
	tag := out["tags"].([]any)[0].(string)
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "git status"})
	res := postOutcome(t, h, map[string]any{"session": sess, "cited": []string{tag}, "stop": true})
	if res["outcomes"].(map[string]any)[tag] != "cited" {
		t.Fatalf("turn 1: %v", res)
	}
	// Cited counts as helpful on the fact.
	if got := factCounts(t, s, fact); got != [2]int{1, 0} {
		t.Fatalf("counters after cite: %v", got)
	}

	// Turn 2: forbidden command runs: violated (even if it also cites).
	ctxJSON(t, h, "should I git push the kestrel repository again", "session", sess)
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "git push origin main"})
	res = postOutcome(t, h, map[string]any{"session": sess, "stop": true})
	if len(res["outcomes"].(map[string]any)) == 0 {
		t.Fatalf("turn 2 produced nothing: %v", res)
	}
	for _, v := range res["outcomes"].(map[string]any) {
		if v != "violated" {
			t.Fatalf("turn 2: %v", res)
		}
	}

	// Turn 3: the forbid held across a turn with tool calls: followed.
	ctxJSON(t, h, "kestrel repository git push question", "session", sess)
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "go test ./..."})
	res = postOutcome(t, h, map[string]any{"session": sess, "stop": true})
	for _, v := range res["outcomes"].(map[string]any) {
		if v != "followed" {
			t.Fatalf("turn 3: %v", res)
		}
	}
	if got := factCounts(t, s, fact); got != [2]int{2, 0} {
		t.Fatalf("counters after follow: %v", got)
	}

	// An item with no check and no citation is ignored, and moves no counter.
	other := remember(t, h, map[string]any{"topic": "zebra", "text": "Zebra feeding schedule runs at dawn"})
	ctxJSON(t, h, "zebra feeding schedule", "session", "sess-cccc-3333")
	res = postOutcome(t, h, map[string]any{"session": "sess-cccc-3333", "stop": true})
	for _, v := range res["outcomes"].(map[string]any) {
		if v != "ignored" {
			t.Fatalf("ignored: %v", res)
		}
	}
	if got := factCounts(t, s, other); got != [2]int{0, 0} {
		t.Fatalf("ignored moved a counter: %v", got)
	}

	rep := do(t, h, "GET", "/api/memory/adherence", nil)
	var report struct {
		Overall  map[string]any   `json:"overall"`
		Memories []map[string]any `json:"memories"`
	}
	decode(t, rep, &report)
	if report.Overall["injected"].(float64) != 4 || report.Overall["cited"].(float64) != 1 ||
		report.Overall["violated"].(float64) != 1 || report.Overall["ignored"].(float64) != 1 || len(report.Memories) != 2 {
		t.Fatalf("report %s", rep.Body)
	}
}

func factCounts(t *testing.T, s *Server, fact map[string]any) [2]int {
	t.Helper()
	hits, err := s.Index.MemoryEntries(indexQueryForID(fact["id"].(string)))
	if err != nil || len(hits) != 1 {
		t.Fatalf("lookup: %v %d", err, len(hits))
	}
	return [2]int{hits[0].Helpful, hits[0].Unhelpful}
}

func TestOutcomeValidatesInput(t *testing.T) {
	_, h := testServer(t)
	for _, b := range []map[string]any{
		{"session": "x"}, {"session": "sess-dddd-4444", "cited": []string{"zz"}},
		{"session": "sess-dddd-4444", "target": strings.Repeat("a", 5000)},
	} {
		if rec := do(t, h, "POST", "/api/memory/outcome", b); rec.Code != 400 {
			t.Errorf("%v -> %d", b, rec.Code)
		}
	}
}

func TestRetellContradictsTheLatestInjection(t *testing.T) {
	s, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact})
	sess := "sess-eeee-5555"
	ctxJSON(t, h, "deploy the web directory in release.conf", "session", sess)
	// The user then has to tell the agent again: remember reports a NOOP re-tell.
	remember(t, h, map[string]any{"topic": "deploy", "text": releaseFact, "context": "ship it"})
	if got := factCounts(t, s, fact); got != [2]int{0, 1} {
		t.Fatalf("counters %v", got)
	}
	postOutcome(t, h, map[string]any{"session": sess, "stop": true})
	rep := do(t, h, "GET", "/api/memory/adherence", nil).Body.String()
	if !strings.Contains(rep, `"contradicted":1`) {
		t.Fatal(rep)
	}
}

func TestEnforceAskReturnsPermissionAtActionStage(t *testing.T) {
	_, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	target := "fact:" + fact["id"].(string)
	if rec := do(t, h, "POST", "/api/memory/check", map[string]any{"target": target, "forbid": `\bgit\s+push\b`, "enforce": "ask", "tools": []string{"Bash"}}); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// Even a query that shares no words with the rule trips it: enforcement is
	// a pattern test on the action, not a relevance score.
	out := ctxJSON(t, h, "Bash git push origin main", "stage", "action", "min_rel", "0.9", "session", "sess-ffff-6666")
	perm, _ := out["permission"].(map[string]any)
	if perm["decision"] != "ask" || !strings.Contains(perm["reason"].(string), "git push") {
		t.Fatalf("no permission: %v", out)
	}
	if !strings.Contains(out["context"].(string), "m:") {
		t.Fatal("rule not injected with its tag")
	}
	if out := ctxJSON(t, h, "Bash git status", "stage", "action", "min_rel", "0.9"); out["permission"] != nil {
		t.Fatalf("benign command asked: %v", out)
	}
	if out := ctxJSON(t, h, "Edit git push notes.txt", "stage", "action"); out["permission"] != nil {
		t.Fatalf("other tool asked: %v", out)
	}
	// The hook's PreToolUse path.
	pre := postOutcome(t, h, map[string]any{"session": "sess-ffff-6666", "tool": "Bash", "target": "git push -f", "pre": true})
	if pre["permission"] == nil {
		t.Fatalf("pre: %v", pre)
	}
	// A bad pattern is refused.
	if rec := do(t, h, "POST", "/api/memory/check", map[string]any{"target": target, "forbid": "("}); rec.Code != 400 {
		t.Fatal("bad regex accepted")
	}
}

func TestIgnoredInjectionsAreDownRankedForInjectionOnly(t *testing.T) {
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "zebra", "text": "Zebra feeding schedule runs at dawn"})
	score := func() float64 {
		rec := do(t, h, "GET", "/api/memory/context?rank=hybrid&min_rel=0&q=zebra+feeding+schedule", nil)
		_ = rec
		return 0
	}
	score()
	for i := 0; i < 22; i++ {
		sess := fmt.Sprintf("sess-zzzz-%04d", i)
		ctxJSON(t, h, "zebra feeding schedule", "session", sess)
		postOutcome(t, h, map[string]any{"session": sess, "stop": true})
	}
	var rep struct {
		Memories []struct {
			Ignored int     `json:"ignored"`
			Penalty float64 `json:"injection_penalty"`
		} `json:"memories"`
	}
	decode(t, do(t, h, "GET", "/api/memory/adherence", nil), &rep)
	if len(rep.Memories) != 1 || rep.Memories[0].Ignored != 22 || rep.Memories[0].Penalty < 0.6 || rep.Memories[0].Penalty > 0.61 {
		t.Fatalf("%+v", rep)
	}
	// Recall is untouched.
	if !strings.Contains(do(t, h, "GET", "/api/memory?q=zebra", nil).Body.String(), "Zebra") {
		t.Fatal("recall lost the fact")
	}
}

func ctxGateServer(delay time.Duration, verdict float64, calls *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(delay)
		var in struct {
			State string `json:"state"`
		}
		json.NewDecoder(r.Body).Decode(&in)
		v := verdict
		if strings.Contains(in.State, "mango") {
			v = 0.9
		}
		fmt.Fprintf(w, `{"answers":{"applies":{"type":"noul","noul":%v}}}`, v)
	}))
}

func TestGateDropsInBandRejectsAndFallsBackOnTimeout(t *testing.T) {
	var calls atomic.Int32
	srv := ctxGateServer(0, 0.1, &calls)
	defer srv.Close()
	s, h := testServer(t)
	remember(t, h, map[string]any{"topic": "zebra", "text": "Zebra feeding schedule runs at dawn today"})
	q := "zebra feeding schedule"
	if got := contextFor(t, h, q, "min_rel", "0.3"); !strings.Contains(got, "Zebra") {
		t.Fatalf("baseline missing: %q", got)
	}
	// Band covers the item's score so the gate runs; model says no.
	s.Settings.Update(map[string]string{"context_gate_url": srv.URL, "context_gate_band": "0.3,1.01"})
	// 1.01 is invalid: falls back to the default band, so widen legitimately.
	s.Settings.Update(map[string]string{"context_gate_band": "0.3,1"})
	if got := contextFor(t, h, q, "min_rel", "0.3"); strings.Contains(got, "Zebra") {
		t.Fatalf("gate said no but memory injected: %q (calls %d)", got, calls.Load())
	}
	// Model says yes for this memory.
	remember(t, h, map[string]any{"topic": "mango", "text": "Mango harvest happens in early summer every year"})
	if got := contextFor(t, h, "mango harvest summer", "min_rel", "0.3"); !strings.Contains(got, "Mango") {
		t.Fatalf("gate yes dropped it: %q", got)
	}
	// A slow model times out inside the budget and the score rule stands.
	slow := ctxGateServer(2*time.Second, 0.1, &calls)
	defer slow.Close()
	s.Settings.Update(map[string]string{"context_gate_url": slow.URL})
	started := time.Now()
	got := contextFor(t, h, q, "min_rel", "0.3")
	if el := time.Since(started); el > 900*time.Millisecond {
		t.Fatalf("gate blew its budget: %v", el)
	}
	if !strings.Contains(got, "Zebra") {
		t.Fatalf("timeout did not fall back to the score rule: %q", got)
	}
	var rep struct {
		Gate struct {
			Calls    int `json:"calls"`
			TimedOut int `json:"timed_out"`
			Dropped  int `json:"dropped"`
		} `json:"gate"`
	}
	decode(t, do(t, h, "GET", "/api/memory/adherence", nil), &rep)
	if rep.Gate.Calls < 3 || rep.Gate.TimedOut != 1 || rep.Gate.Dropped < 1 {
		t.Fatalf("gate telemetry %+v", rep.Gate)
	}
}

func TestProposeChecksStoresSuggestionsNeverApplies(t *testing.T) {
	var asked atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		fmt.Fprint(w, `{"answers":{"detector":{"type":"noul","noul":0.9}}}`)
	}))
	defer srv.Close()
	s, h := testServer(t)
	s.Settings.Update(map[string]string{"decision_url": srv.URL})
	fact := remember(t, h, map[string]any{"topic": "git", "text": "Do not run `git push --force` on shared branches"})
	remember(t, h, map[string]any{"topic": "style", "text": "Use tabs in Go files"})
	rec := do(t, h, "POST", "/api/memory/check/propose", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"proposed":1`) {
		t.Fatal(rec.Body.String())
	}
	if strings.Contains(do(t, h, "GET", "/api/memory/check", nil).Body.String(), "forbid") {
		t.Fatal("a proposal was applied")
	}
	sug := do(t, h, "GET", "/api/memory/check?suggestions=1", nil).Body.String()
	if !regexp.MustCompile(`git\\\\s\+push`).MatchString(sug) && !strings.Contains(sug, "push") {
		t.Fatalf("suggestion missing: %s", sug)
	}
	target := "fact:" + fact["id"].(string)
	if rec := do(t, h, "POST", "/api/memory/check/accept", map[string]any{"target": target, "enforce": "ask"}); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	if !strings.Contains(do(t, h, "GET", "/api/memory/check", nil).Body.String(), `"enforce":"ask"`) {
		t.Fatal("accept did not store the check")
	}
}

func indexQueryForID(id string) index.MemoryQuery {
	return index.MemoryQuery{ID: id, Limit: 1, Now: vault.Now()}
}
