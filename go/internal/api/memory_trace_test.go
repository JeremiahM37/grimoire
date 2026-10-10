package api

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

type traceCard struct {
	Card struct {
		Exposures int    `json:"exposures"`
		Withheld  int    `json:"withheld"`
		Kind      string `json:"kind"`
		Uptake    struct{ K, N int }
		Influence struct{ K, N int }
		Evidence  map[string]int `json:"link_evidence"`
		Reminders struct {
			Changed, Unchanged, Abandoned int
		}
		Linked struct {
			Actions   int `json:"actions"`
			Failed    int `json:"failed"`
			Corrected int `json:"corrected"`
		} `json:"linked_actions"`
		Benefit struct {
			Label string
		}
	} `json:"card"`
	Causal causalView `json:"causal"`
	Labels map[string]string
}

func getTrace(t *testing.T, h http.Handler, target string) traceCard {
	t.Helper()
	rec := do(t, h, "GET", "/api/memory/trace?target="+target, nil)
	if rec.Code != 200 {
		t.Fatalf("trace %d %s", rec.Code, rec.Body)
	}
	var out traceCard
	decode(t, rec, &out)
	return out
}

func actionCtx(t *testing.T, h http.Handler, session, tu, cmd string) map[string]any {
	t.Helper()
	return ctxJSON(t, h, "Bash "+cmd, "session", session, "stage", "action", "tu", tu, "min_rel", "0.3")
}

func TestTraceCardLinksActionsAndRecordsTheReminderOutcome(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	fillStore(t, h, 80)
	fact := remember(t, h, map[string]any{"topic": "zebra", "text": zebraFact})
	target := "fact:" + fact["id"].(string)
	sess := "sess-trace-0001"

	out := actionCtx(t, h, sess, "toolu_01", "zebra feeding schedule dawn")
	tags, _ := out["tags"].([]any)
	if len(tags) != 1 {
		t.Fatalf("no injection: %v", out)
	}
	tag := tags[0].(string)
	// The agent changes its pending action to touch the file the memory names;
	// the call carries one of the memory's fingerprints.
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "vi /srv/zebra/feeder-quokka.yaml",
		"tu": "toolu_01", "err": 0, "ev": map[string]int{tag: 2}, "fp": map[string]int{tag: 2}})
	// A later failing call in the same session, not linked.
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "go test ./...", "tu": "toolu_02",
		"err": 1, "exit": 1, "tp": 3, "tf": 2})
	postOutcome(t, h, map[string]any{"session": sess, "stop": true})

	got := getTrace(t, h, target)
	c := got.Card
	if c.Exposures != 1 || c.Evidence["fp"] != 1 || c.Evidence["changed"] != 1 {
		t.Fatalf("card: %+v", c)
	}
	if c.Reminders.Changed != 1 || c.Linked.Actions != 1 || c.Uptake.K != 1 || c.Uptake.N != 1 || c.Influence.K != 1 {
		t.Fatalf("card: %+v", c)
	}
	if c.Benefit.Label != "associated" || got.Causal.Status != "holdout off" || got.Causal.Label != "" {
		t.Fatalf("labels: %+v", got)
	}
	if got.Labels["causal"] == "" || !strings.Contains(got.Labels["benefit"], "associated") {
		t.Fatalf("labels missing: %v", got.Labels)
	}

	if rec := do(t, h, "GET", "/api/memory/trace?target=fact:ffffffffffff", nil); rec.Code != 404 {
		t.Fatalf("unknown target: %d", rec.Code)
	}
	if rec := do(t, h, "GET", "/api/memory/trace?target=bogus", nil); rec.Code != 400 {
		t.Fatalf("bad target: %d", rec.Code)
	}
}

func TestOutcomeTraceFieldsAreValidated(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	for name, body := range map[string]map[string]any{
		"bad tool_use id":  {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "tu": "a b;c"},
		"bad region":       {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "region": "ZZ"},
		"bad reedit":       {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "reedit": []string{"nothex"}},
		"err out of range": {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "err": 2},
		"ev out of range":  {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "ev": map[string]int{"3e99": 99}},
		"ev bad tag":       {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "ev": map[string]int{"XYZ": 1}},
		"meaning range":    {"session": "sess-valid-0001", "tool": "Bash", "target": "ls", "meaning": map[string]float64{"3e99": 3}},
	} {
		if rec := do(t, h, "POST", "/api/memory/outcome", body); rec.Code != 400 {
			t.Errorf("%s accepted: %d", name, rec.Code)
		}
	}
	// A prompt event takes no tool.
	postOutcome(t, h, map[string]any{"session": "sess-valid-0001", "prompt": true, "corr": true})
}

func TestServerSideRetellMarksThePreviousTurnCorrected(t *testing.T) {
	_, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	target := "fact:" + fact["id"].(string)
	old := adherence.PromptMergeWindow
	adherence.PromptMergeWindow = 0
	t.Cleanup(func() { adherence.PromptMergeWindow = old })
	sess := "sess-retell-0001"
	out := ctxJSON(t, h, "should I git push the kestrel repository", "session", sess)
	tag := out["tags"].([]any)[0].(string)
	postOutcome(t, h, map[string]any{"session": sess, "tool": "Bash", "target": "git push origin main", "err": 0,
		"ev": map[string]int{tag: 1}, "cited": []string{tag}})
	time.Sleep(1100 * time.Millisecond)
	// The next prompt restates the rule: the previous turn's actions were corrected.
	ctxJSON(t, h, "I already told you never to git push the kestrel repository", "session", sess)
	got := getTrace(t, h, target)
	if got.Card.Linked.Actions != 1 || got.Card.Linked.Corrected != 1 {
		t.Fatalf("linked %+v", got.Card.Linked)
	}
}

// ---- holdout ----

func setHoldout(t *testing.T, s *Server, rate string, draw float64) {
	t.Helper()
	if err := s.Settings.Update(map[string]string{"memory_holdout_rate": rate, "memory_trace_min_arm": "2"}); err != nil {
		t.Fatal(err)
	}
	s.holdRand = func() float64 { return draw }
}

func TestHoldoutWithholdsEligibleItemsAndLogsPropensity(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	fillStore(t, h, 40)
	fact := remember(t, h, map[string]any{"topic": "zebra", "text": zebraFact})
	target := "fact:" + fact["id"].(string)

	// Off: nothing changes and nothing is withheld.
	out := ctxJSON(t, h, "zebra feeding schedule", "session", "sess-hold-0001")
	if len(out["tags"].([]any)) != 1 || getTrace(t, h, target).Card.Withheld != 0 {
		t.Fatalf("off: %v", out)
	}

	setHoldout(t, s, "0.3", 0.0) // every draw falls inside the rate
	out = ctxJSON(t, h, "zebra feeding schedule", "session", "sess-hold-0002")
	if out["context"] != "" || len(out["tags"].([]any)) != 0 {
		t.Fatalf("withheld item was injected: %v", out)
	}
	keys := out["keys"].([]any)
	if len(keys) != 1 {
		t.Fatalf("keys must still list the withheld item so the hook dedups it: %v", out)
	}
	if out["fp"] != nil {
		if items := out["fp"].(map[string]any)["items"].(map[string]any); len(items) != 0 {
			t.Fatalf("fingerprints were sent for a withheld item: %v", out["fp"])
		}
	}
	got := getTrace(t, h, target)
	if got.Card.Withheld != 1 || got.Card.Exposures != 1 {
		t.Fatalf("withheld row: %+v", got.Card)
	}
	// The withheld item left no injection row: adherence counts are untouched.
	var rep struct{ Overall map[string]any }
	decode(t, do(t, h, "GET", "/api/memory/adherence", nil), &rep)
	if rep.Overall["injected"].(float64) != 1 {
		t.Fatalf("adherence counted a withheld item: %v", rep.Overall)
	}

	// A draw outside the rate shows the item and logs the propensity it faced.
	s.holdRand = func() float64 { return 0.99 }
	out = ctxJSON(t, h, "zebra feeding schedule", "session", "sess-hold-0003")
	if len(out["tags"].([]any)) != 1 {
		t.Fatalf("shown: %v", out)
	}
	postOutcome(t, h, map[string]any{"session": "sess-hold-0003", "tool": "Bash", "target": "ls", "err": 0})
	postOutcome(t, h, map[string]any{"session": "sess-hold-0002", "tool": "Bash", "target": "ls", "err": 1})
	recs, err := s.adh().Records(target, time.Now().Add(-time.Hour), 0)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records %v %v", recs, err)
	}
	for _, r := range recs {
		if r.PWithhold != 0.3 || r.Kind == "" {
			t.Fatalf("%+v", r)
		}
		if r.Treated == (r.Y == 1) {
			t.Fatalf("outcome attached to the wrong arm: %+v", r)
		}
	}
}

func TestHoldoutNeverWithholdsPinnedEnforcedOrCompiledRules(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	fillStore(t, h, 40)
	pinned := remember(t, h, map[string]any{"topic": "zebra", "text": zebraFact, "immutable": true})
	rule := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	if rec := do(t, h, "POST", "/api/memory/check", map[string]any{"target": "fact:" + rule["id"].(string),
		"forbid": `\bgit\s+push\b`, "enforce": "ask"}); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	plain := remember(t, h, map[string]any{"topic": "otters", "text": "Otter enclosure cleaning happens on Tuesdays, see /srv/otters/schedule-marmot.csv"})
	setHoldout(t, s, "0.5", 0.0)
	for _, c := range []struct {
		q, name string
		want    int
	}{{"zebra feeding schedule", "pinned", 1}, {"should I git push the kestrel repository", "enforced rule", 1}, {"otter enclosure cleaning tuesdays", "plain fact", 0}} {
		out := ctxJSON(t, h, c.q, "session", "sess-prot-"+c.name[:3]+"1")
		if got := len(out["tags"].([]any)); got != c.want {
			t.Errorf("%s: %d items shown, want %d (%v)", c.name, got, c.want, out["context"])
		}
	}
	_ = plain
	// An action that trips an enforce decision is shown, whatever the draw.
	out := ctxJSON(t, h, "Bash git push origin main", "session", "sess-prot-act1", "stage", "action", "min_rel", "0.3")
	if len(out["tags"].([]any)) != 1 || out["permission"] == nil {
		t.Fatalf("enforced action withheld: %v", out)
	}
	card := getTrace(t, h, "fact:"+rule["id"].(string)).Card
	if card.Withheld != 0 || card.Reminders.Unchanged+card.Reminders.Changed+card.Reminders.Abandoned != 0 {
		t.Logf("card %+v", card)
	}
	if got := getTrace(t, h, "fact:"+pinned["id"].(string)).Card.Withheld; got != 0 {
		t.Fatalf("pinned withheld %d", got)
	}
}

type fakeEstimator struct {
	got []utilization.Record
	err error
}

func (f *fakeEstimator) Name() string { return "fake" }
func (f *fakeEstimator) Lift(r []utilization.Record) (utilization.Estimate, error) {
	f.got = r
	return utilization.Estimate{Method: "fake", Effect: -0.2, Lo: -0.4, Hi: 0.0, N: len(r)}, f.err
}

func TestCausalStatusesAndTheEstimatorInterface(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	fillStore(t, h, 40)
	fact := remember(t, h, map[string]any{"topic": "zebra", "text": zebraFact})
	target := "fact:" + fact["id"].(string)
	setHoldout(t, s, "0.2", 0.5)
	run := func(session string, draw float64, err int) {
		s.holdRand = func() float64 { return draw }
		ctxJSON(t, h, "zebra feeding schedule", "session", session)
		postOutcome(t, h, map[string]any{"session": session, "tool": "Bash", "target": "ls", "err": err})
	}
	run("sess-cau-0001", 0.9, 0) // shown
	if got := getTrace(t, h, target).Causal; got.Status != "insufficient data" || got.Label != "" || got.Treated != 1 {
		t.Fatalf("insufficient: %+v", got)
	}
	run("sess-cau-0002", 0.9, 0)
	run("sess-cau-0003", 0.0, 1) // withheld
	run("sess-cau-0004", 0.0, 1)
	got := getTrace(t, h, target).Causal
	if got.Status != "estimator not installed" || got.Treated != 2 || got.Withheld != 2 || got.Label != "" {
		t.Fatalf("no estimator: %+v", got)
	}
	fe := &fakeEstimator{}
	s.Estimator = fe
	got = getTrace(t, h, target).Causal
	if got.Status != "estimated" || got.Label != "caused" || got.Estimate == nil || got.Estimate.Lo >= got.Estimate.Hi || got.Estimator != "fake" {
		t.Fatalf("estimated: %+v", got)
	}
	if len(fe.got) != 4 {
		t.Fatalf("estimator saw %d records", len(fe.got))
	}
	s.Estimator = &fakeEstimator{err: errors.New("boom")}
	if got = getTrace(t, h, target).Causal; got.Status != "estimator error" || got.Label != "" {
		t.Fatalf("error: %+v", got)
	}

	// Summary: admin-only roll-up by kind with the same labels.
	rec := do(t, h, "GET", "/api/memory/trace/summary", nil)
	if rec.Code != 200 {
		t.Fatalf("summary %d %s", rec.Code, rec.Body)
	}
	var sum struct {
		HoldoutRate float64 `json:"holdout_rate"`
		Kinds       []struct {
			Kind      string
			Exposures int
			Withheld  int
			Causal    causalView
		}
	}
	decode(t, rec, &sum)
	if sum.HoldoutRate != 0.2 || len(sum.Kinds) < 2 || sum.Kinds[0].Kind != "" || sum.Kinds[0].Exposures != 2 || sum.Kinds[0].Withheld != 2 {
		t.Fatalf("summary %s", rec.Body)
	}
}
