package adherence

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func traceStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func inj(session, tag, target, kind string) Injection {
	return Injection{Session: session, Tag: tag, Key: tag + "0000", Target: target, Stage: "prompt", Kind: kind, FPKnown: true, FPN: 2}
}

func TestInfluenceLinksOnUptakeEvidenceInTheWindow(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	st.Log([]Injection{inj("s1", "aaaa", "fact:one", "rule"), inj("s1", "bbbb", "fact:two", "fact")}, now.Add(-time.Minute))
	// Fingerprint evidence for aaaa only; bbbb is merely present.
	id, linked, err := st.RecordAction(ActionIn{Session: "s1", Tool: "Bash", Target: "ls", Failed: 0, TestsPass: -1, TestsFail: -1,
		Ev: map[string]int{"aaaa": 2}, TS: now})
	if err != nil || len(linked) != 1 || linked[0].Tag != "aaaa" {
		t.Fatalf("linked %v err %v", linked, err)
	}
	// A tag carried by a later action links bbbb, and a tag outranks a fingerprint.
	id2, linked, _ := st.RecordAction(ActionIn{Session: "s1", Tool: "Bash", Target: "git commit -m x (m:bbbb)", Failed: 0,
		TestsPass: -1, TestsFail: -1, Cited: []string{"bbbb"}, Ev: map[string]int{"bbbb": 1}, TS: now.Add(time.Second)})
	if len(linked) != 1 || linked[0].Tag != "bbbb" || id2 == id {
		t.Fatalf("second link %v", linked)
	}
	var tagLinks int
	st.db.QueryRow(`SELECT COUNT(*) FROM trace_links WHERE action_id=? AND evidence=?`, id2, EvTag).Scan(&tagLinks)
	if tagLinks != 1 {
		t.Fatalf("tag evidence rows: %d", tagLinks)
	}
	// Outside the window nothing links.
	st.Log([]Injection{inj("s2", "cccc", "fact:three", "fact")}, now.Add(-3*time.Hour))
	_, linked, _ = st.RecordAction(ActionIn{Session: "s2", Tool: "Bash", Target: "ls", Failed: 0, TestsPass: -1, TestsFail: -1,
		Ev: map[string]int{"cccc": 1}, TS: now})
	if len(linked) != 0 {
		t.Fatalf("stale exposure linked: %v", linked)
	}
	// A repeated report of one tool_use id is one action.
	for i := 0; i < 2; i++ {
		st.RecordAction(ActionIn{Session: "s3", TU: "toolu_1", Tool: "Bash", Target: "x", Failed: -1, TestsPass: -1, TestsFail: -1, TS: now})
	}
	var n int
	st.db.QueryRow(`SELECT COUNT(*) FROM trace_actions WHERE session='s3'`).Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate action rows: %d", n)
	}
}

func reminder(session, tag, target, tool, cmd, tu string, ask bool) Injection {
	i := inj(session, tag, target, "rule")
	i.Stage, i.Tool, i.Pend, i.TU, i.Ask = "action", tool, PendingHash(tool, cmd), tu, ask
	return i
}

func remOf(t *testing.T, st *Store, tag string) string {
	t.Helper()
	var rem string
	st.db.QueryRow(`SELECT rem FROM injections WHERE tag=?`, tag).Scan(&rem)
	return rem
}

func TestPendingActionNaturalExperiment(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	run := func(session, tu, cmd string, at time.Time) int64 {
		id, _, err := st.RecordAction(ActionIn{Session: session, TU: tu, Tool: "Bash", Target: cmd, Failed: 0, TestsPass: -1, TestsFail: -1, TS: at})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	// Ran unchanged.
	st.Log([]Injection{reminder("s1", "aaaa", "fact:a", "Bash", "git push origin main", "", false)}, now)
	run("s1", "", "git push origin main", now.Add(2*time.Second))
	if got := remOf(t, st, "aaaa"); got != RemUnchanged {
		t.Fatalf("unchanged: %q", got)
	}
	// Changed after the reminder: the next executed action differs, and it is
	// linked as strong influence evidence.
	st.Log([]Injection{reminder("s2", "bbbb", "fact:b", "Bash", "git push --force", "", false)}, now)
	id := run("s2", "", "git push --force-with-lease", now.Add(2*time.Second))
	if got := remOf(t, st, "bbbb"); got != RemChanged {
		t.Fatalf("changed: %q", got)
	}
	var ev string
	if err := st.db.QueryRow(`SELECT evidence FROM trace_links WHERE action_id=?`, id).Scan(&ev); err != nil || ev != EvChanged {
		t.Fatalf("changed link: %q %v", ev, err)
	}
	// Never executed: abandoned at the end of the turn. An ask rule's decision is recorded.
	st.Log([]Injection{reminder("s3", "cccc", "fact:c", "Bash", "rm -rf build", "", true)}, now)
	if n, _ := st.FinishReminders("s3"); n != 1 || remOf(t, st, "cccc") != RemAbandoned {
		t.Fatalf("abandoned: %q", remOf(t, st, "cccc"))
	}
	// Parallel calls with tool_use ids: another call's completion is not a change.
	st.Log([]Injection{reminder("s4", "dddd", "fact:d", "Bash", "make deploy", "tu-2", false)}, now)
	run("s4", "tu-1", "ls", now.Add(time.Second))
	if got := remOf(t, st, "dddd"); got != "" {
		t.Fatalf("parallel call resolved the reminder: %q", got)
	}
	run("s4", "tu-2", "make deploy", now.Add(2*time.Second))
	if got := remOf(t, st, "dddd"); got != RemUnchanged {
		t.Fatalf("by id: %q", got)
	}
	// Without ids a call matching ANY pending reminder resolves that one, not its neighbour.
	st.Log([]Injection{reminder("s5", "eeee", "fact:e", "Bash", "cmd one", "", false), reminder("s5", "ffff", "fact:f", "Bash", "cmd two", "", false)}, now)
	run("s5", "", "cmd two", now.Add(time.Second))
	if remOf(t, st, "ffff") != RemUnchanged || remOf(t, st, "eeee") != "" {
		t.Fatalf("sequence match: e=%q f=%q", remOf(t, st, "eeee"), remOf(t, st, "ffff"))
	}
	// An edit pending for one file followed by an edit of another file is a change
	// even across tools (the next executed action differs).
	st.Log([]Injection{reminder("s6", "1111", "fact:g", "Edit", "/a/x.go", "", false)}, now)
	run("s6", "", "go test ./...", now.Add(time.Second))
	if remOf(t, st, "1111") != RemChanged {
		t.Fatalf("cross-tool change: %q", remOf(t, st, "1111"))
	}

	card, _ := st.Card("fact:a", now.Add(-time.Hour))
	if card.Reminders.Unchanged != 1 || card.Reminders.Change.N != 1 {
		t.Fatalf("card reminders: %+v", card.Reminders)
	}
	cc, _ := st.Card("fact:c", now.Add(-time.Hour))
	if cc.Reminders.Ask != 1 || cc.Reminders.AskNot != 1 || cc.Reminders.Abandoned != 1 {
		t.Fatalf("ask stats: %+v", cc.Reminders)
	}
}

func TestOutcomeCodesReeditRevertCorrection(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	add := func(a ActionIn) int64 {
		a.TestsPass, a.TestsFail = -1, -1
		if a.Failed == 0 {
			a.Failed = 0
		}
		id, _, err := st.RecordAction(a)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	get := func(id int64, col string) int {
		var v int
		st.db.QueryRow(`SELECT `+col+` FROM trace_actions WHERE id=?`, id).Scan(&v)
		return v
	}
	e1 := add(ActionIn{Session: "s", Tool: "Edit", Target: "/a.go", Region: "aaaaaaaa11111111", TS: now})
	e2 := add(ActionIn{Session: "s", Tool: "Edit", Target: "/a.go", Region: "bbbbbbbb22222222", Reedit: []string{"aaaaaaaa11111111"}, TS: now.Add(time.Second)})
	add(ActionIn{Session: "s", Tool: "Edit", Target: "/a.go", Region: "cccccccc33333333", Revert: []string{"bbbbbbbb22222222"}, TS: now.Add(2 * time.Second)})
	if get(e1, "reedit") != 1 || get(e2, "revert") != 1 || get(e1, "revert") != 0 {
		t.Fatalf("regions: e1 reedit=%d e2 revert=%d", get(e1, "reedit"), get(e2, "revert"))
	}
	// A prompt that corrects marks the previous turn's actions, once, whether
	// the hook or the server reports it first.
	t0 := now.Add(time.Minute)
	st.TracePrompt("s", false, t0) // prompt 1
	a1 := add(ActionIn{Session: "s", Tool: "Bash", Target: "x", Failed: 0, TS: t0.Add(2 * time.Second)})
	t1 := t0.Add(30 * time.Second)
	st.TracePrompt("s", false, t1) // server saw prompt 2, no restatement
	if n, _ := st.TracePrompt("s", true, t1.Add(time.Second)); n != 1 {
		t.Fatalf("hook verdict marked %d actions", n)
	}
	if get(a1, "correction") != 1 {
		t.Fatal("previous turn's action not marked corrected")
	}
	a2 := add(ActionIn{Session: "s", Tool: "Bash", Target: "y", Failed: 0, TS: t1.Add(10 * time.Second)})
	if get(a2, "correction") != 0 {
		t.Fatal("the corrected prompt's own turn was marked")
	}
}

func TestBenefitIsStratifiedAndLabelledAssociated(t *testing.T) {
	// Influenced: 20 mid Bash actions with 2 bad; comparison: 100 with 30 bad in
	// the same stratum (a different stratum with no influenced actions is ignored).
	inf := map[strataKey]stratum{{"Bash", "mid"}: {20, 2}}
	ctl := map[strataKey]stratum{{"Bash", "mid"}: {100, 30}, {"Edit", "late"}: {500, 400}}
	b := MatchedDiff(inf, ctl)
	if b.Label != "associated" || b.Matched != 20 || b.Comparison != 100 || b.Strata != 1 {
		t.Fatalf("%+v", b)
	}
	if math.Abs(b.Diff-(0.1-0.3)) > 1e-9 || b.Lo >= b.Diff || b.Hi <= b.Diff {
		t.Fatalf("diff/interval: %+v", b)
	}
	if b.Lo >= 0 || b.Hi >= 0.05 { // 20 vs 100 actions: clearly lower, interval excludes 0
		t.Fatalf("interval should exclude zero: %+v", b)
	}
	none := MatchedDiff(inf, map[strataKey]stratum{{"Edit", "late"}: {10, 1}})
	if none.Note == "" || none.Comparison != 0 {
		t.Fatalf("no matched stratum must say so: %+v", none)
	}
	w := Wilson(0, 20)
	if w.Lo != 0 || w.Hi < 0.1 || w.Hi > 0.2 {
		t.Fatalf("wilson(0,20): %+v", w)
	}
}

func TestBenefitFromTheStoreMatchesInSessionStage(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	st.Log([]Injection{inj("s1", "aaaa", "fact:one", "rule")}, now.Add(-time.Hour))
	mk := func(i int, bad bool, link bool) {
		failed := 0
		if bad {
			failed = 1
		}
		a := ActionIn{Session: "s1", Tool: "Bash", Target: "cmd" + string(rune('a'+i)), Failed: failed, TestsPass: -1, TestsFail: -1, TS: now.Add(-time.Duration(30-i) * time.Minute)}
		if link {
			a.Ev = map[string]int{"aaaa": 1}
		}
		st.RecordAction(a)
	}
	for i := 0; i < 8; i++ { // first 8 are in the "early" stage: 4 influenced (1 bad), 4 not (3 bad)
		mk(i, (i < 4 && i == 0) || (i >= 4 && i != 4), i < 4)
	}
	c, err := st.Card("fact:one", now.Add(-2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if c.Benefit.Influenced != 4 || c.Benefit.Comparison != 4 || c.Benefit.Label != "associated" {
		t.Fatalf("%+v", c.Benefit)
	}
	if math.Abs(c.Benefit.Diff-(0.25-0.75)) > 1e-9 {
		t.Fatalf("diff %v", c.Benefit.Diff)
	}
	if c.Influence.N != 0 { // Tools never bumped (no outcome call): not in the denominator
		t.Logf("influence denominator %d", c.Influence.N)
	}
	if c.Linked.Actions != 4 || c.Linked.Failed != 1 || c.Linked.BadActions != 1 {
		t.Fatalf("linked outcomes %+v", c.Linked)
	}
}

func TestRecordsForTheEstimator(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	shown := inj("s1", "aaaa", "fact:one", "rule")
	shown.PWithhold = 0.1
	st.Log([]Injection{shown}, now.Add(-time.Minute))
	st.LogWithheld([]Withheld{{Session: "s2", Tag: "bbbb", Key: "bbbb0000", Target: "fact:one", Kind: "rule", Stage: "prompt", PWithhold: 0.1}}, now.Add(-time.Minute))
	// An always-shown item (never eligible) is not a record.
	st.Log([]Injection{inj("s1", "cccc", "fact:two", "rule")}, now.Add(-time.Minute))
	for _, s := range []string{"s1", "s2"} {
		failed := 0
		if s == "s2" {
			failed = 1
		}
		st.RecordAction(ActionIn{Session: s, Tool: "Bash", Target: "x", Failed: failed, TestsPass: -1, TestsFail: -1, TS: now})
	}
	// A decision with no observable action after it is dropped.
	st.LogWithheld([]Withheld{{Session: "s9", Tag: "dddd", Key: "dddd0000", Target: "fact:one", PWithhold: 0.1}}, now)
	recs, err := st.Records("fact:one", now.Add(-time.Hour), 0)
	if err != nil || len(recs) != 2 {
		t.Fatalf("records %v %v", recs, err)
	}
	for _, r := range recs {
		if r.PWithhold != 0.1 || r.Actions != 1 || r.Kind != "rule" {
			t.Fatalf("%+v", r)
		}
		if r.Treated && r.Y != 0 || !r.Treated && r.Y != 1 {
			t.Fatalf("outcome %+v", r)
		}
	}
}

func TestTraceKeepsNoTextAndRetentionIsBounded(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	secret := "SECRET-COMMAND-TEXT --token hunter2"
	st.Log([]Injection{inj("s1", "aaaa", "fact:one", "rule")}, now.Add(-31*24*time.Hour))
	st.LogWithheld([]Withheld{{Session: "s1", Tag: "bbbb", Key: "bbbb0000", Target: "fact:one", PWithhold: 0.1}}, now.Add(-31*24*time.Hour))
	old := now.Add(-31 * 24 * time.Hour)
	st.RecordAction(ActionIn{Session: "s1", Tool: "Bash", Target: secret, Failed: 0, TestsPass: -1, TestsFail: -1, Ev: map[string]int{"aaaa": 1}, TS: old})
	st.RecordAction(ActionIn{Session: "s1", Tool: "Bash", Target: secret, Failed: 0, TestsPass: -1, TestsFail: -1, TS: now})
	// The next write that prunes drops everything past 30 days, links included.
	st.mu.Lock()
	st.lastPrune = time.Time{}
	st.mu.Unlock()
	st.Log([]Injection{inj("s1", "dddd", "fact:two", "fact")}, now)
	for table, want := range map[string]int{"injections": 1, "trace_actions": 1, "trace_withheld": 0, "trace_links": 0} {
		var n int
		st.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n)
		if n != want {
			t.Errorf("%s has %d rows, want %d", table, n, want)
		}
	}
	st.Close()
	for _, f := range []string{"adherence.db", "adherence.db-wal"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err == nil && (strings.Contains(string(b), "SECRET-COMMAND-TEXT") || strings.Contains(string(b), "hunter2")) {
			t.Fatalf("%s holds the command text", f)
		}
	}
}

// A different edit of the same file must not read as "unchanged": the pending
// hash folds in the hook's hash of the edit content.
func TestPendingHashSeparatesEditsOfOneFile(t *testing.T) {
	if PendingHashEC("Edit", "/a.go", "") != PendingHash("Edit", "/a.go") {
		t.Fatal("empty content hash must equal the plain pending hash")
	}
	if PendingHashEC("Edit", "/a.go", "aaaa1111aaaa1111") == PendingHashEC("Edit", "/a.go", "bbbb2222bbbb2222") {
		t.Fatal("content hash ignored")
	}
	st := traceStore(t)
	now := time.Now()
	rec := func(session, tag, ec string) Injection {
		i := reminder(session, tag, "fact:"+tag, "Edit", "/a.go", "", false)
		i.Pend = PendingHashEC("Edit", "/a.go", ec)
		return i
	}
	act := func(session, ec string) int64 {
		id, _, err := st.RecordAction(ActionIn{Session: session, Tool: "Edit", Target: "/a.go", EC: ec, Failed: 0, TestsPass: -1, TestsFail: -1, TS: now.Add(2 * time.Second)})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	st.Log([]Injection{rec("e1", "e1e1", "aaaa1111aaaa1111")}, now)
	act("e1", "aaaa1111aaaa1111")
	if got := remOf(t, st, "e1e1"); got != RemUnchanged {
		t.Fatalf("same edit: %q", got)
	}
	st.Log([]Injection{rec("e2", "e2e2", "aaaa1111aaaa1111")}, now)
	act("e2", "bbbb2222bbbb2222")
	if got := remOf(t, st, "e2e2"); got != RemChanged {
		t.Fatalf("different edit of the same file: %q", got)
	}
}

func TestCardListsRecentLinkedActionsAsCodesOnly(t *testing.T) {
	st := traceStore(t)
	now := time.Now()
	st.Log([]Injection{inj("r1", "dddd", "fact:recent", "rule")}, now.Add(-time.Minute))
	for i := 0; i < RecentActions+3; i++ {
		failed := i % 2
		if _, _, err := st.RecordAction(ActionIn{Session: "r1", Tool: "Edit", Target: "/secret/path.go", Failed: failed, TestsPass: -1, TestsFail: -1,
			Ev: map[string]int{"dddd": 1}, TS: now.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	c, err := st.Card("fact:recent", now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Recent) != RecentActions || c.Recent[0].Seq <= c.Recent[1].Seq || c.Recent[0].Tool != "Edit" {
		t.Fatalf("recent actions: %+v", c.Recent)
	}
	empty, _ := st.Card("fact:none", now.Add(-time.Hour))
	if empty.Recent == nil {
		t.Fatal("recent_actions must be [] not null")
	}
}
