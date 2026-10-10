package rulecheck

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func first(t *testing.T, text, shape string) Candidate {
	t.Helper()
	for _, c := range Deterministic(text) {
		if c.Shape == shape {
			return c
		}
	}
	t.Fatalf("no %s candidate for %q: %+v", shape, text, Deterministic(text))
	return Candidate{}
}

func TestDeterministicShapes(t *testing.T) {
	c := first(t, "Never run `git push --force` on main.", ShapeForbid)
	cc, err := c.Spec.Compile()
	if err != nil || !cc.Violates("git push --force origin main", nil) || cc.Violates("git status", nil) {
		t.Fatalf("forbid: %v %+v", err, c)
	}
	b := first(t, "Always run `go test` before `git push`.", ShapeRequireBefore)
	bc, err := b.Spec.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if !bc.Violates("git push origin x", nil) || bc.Violates("git push origin x", []string{"go test ./..."}) || bc.Violates("go test ./... && git push", nil) {
		t.Fatalf("require_before wrong: %+v", b)
	}
	b2 := first(t, "Before `git push`, run `go test`.", ShapeRequireBefore)
	if b2.Action != b.Action || b2.Before != b.Before {
		t.Fatalf("before-first phrasing differs: %+v vs %+v", b2, b)
	}
	w := first(t, "Always pass `-y` to apt install.", ShapeRequireWith)
	wc, err := w.Spec.Compile()
	if err != nil || !wc.Violates("apt install foo", nil) || wc.Violates("apt install -y foo", nil) {
		t.Fatalf("require_with: %v %+v", err, w)
	}
}

func TestScopeAndFilePath(t *testing.T) {
	c := first(t, "Never edit `/etc/hosts`.", ShapeForbid)
	if len(c.Tools) == 0 || c.Tools[0] != "Edit" {
		t.Fatalf("path rule should target file tools: %+v", c)
	}
	s := first(t, "In the lectern repo never run `make deploy`.", ShapeForbid)
	cc, _ := s.Spec.Compile()
	if !cc.AppliesTo("Bash", "/home/a/projects/lectern/x", "") || cc.AppliesTo("Bash", "/home/a/projects/other", "") {
		t.Fatalf("scope: %+v", s.Scope)
	}
}

func TestNoCandidateForStyleRules(t *testing.T) {
	if got := Deterministic("Keep answers short and plain."); len(got) != 0 {
		t.Fatalf("style rule compiled: %+v", got)
	}
}

func TestCompileRejectsUnsafe(t *testing.T) {
	for _, sp := range []Spec{
		{Shape: ShapeForbid, Action: ".*"},
		{Shape: ShapeForbid, Action: "(a"},
		{Shape: ShapeForbid, Action: "x*"},
		{Shape: ShapeForbid, Action: strings.Repeat("a", MaxPattern+1)},
		{Shape: ShapeForbid, Action: `(?=a)b`},
		{Shape: ShapeRequireBefore, Action: "a"},
		{Shape: "nope", Action: "a"},
	} {
		if _, err := sp.Compile(); err == nil {
			t.Errorf("accepted %+v", sp)
		}
	}
}

func TestCatastrophicPatternIsLinear(t *testing.T) {
	c, err := Spec{Shape: ForbidShape(), Action: `(a+)+$`}.Compile()
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	c.Violates(strings.Repeat("a", 3000)+"!", nil)
	if time.Since(start) > time.Second {
		t.Fatal("RE2 should not backtrack")
	}
}

func ForbidShape() string { return ShapeForbid }

func TestParseLLM(t *testing.T) {
	got, err := ParseLLM(`{"checks":[{"shape":"forbid","action":"\\bgit\\s+push\\b","why":"x"},{"shape":"forbid","action":"(?=x)"},{"shape":"require_with","action":"apt","with":""}]}`)
	if err != nil || len(got) != 1 || got[0].Source != "llm" {
		t.Fatalf("%v %+v", err, got)
	}
	if _, err := ParseLLM("not json"); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestWilsonAndPolicy(t *testing.T) {
	lo, hi := Wilson(9, 10)
	if lo < 0.55 || lo > 0.6 || hi < 0.98 {
		t.Fatalf("wilson %v %v", lo, hi)
	}
	p := DefaultPolicy()
	ev := func(l, tr int, text, user, prev string) string {
		s, _ := p.Decide(Evidence{RuleText: text, Labelled: l, True: tr, Matches: 30, MatchRate: 0.01, User: user, Previous: prev})
		return s
	}
	if ev(4, 4, "never push", "", "") != StatusSuggestion {
		t.Error("needs 5 labels")
	}
	if ev(10, 9, "prefer small commits", "", "") != StatusActive {
		t.Error("0.9 should be a reminder")
	}
	if ev(20, 19, "Never push", "", "") != StatusEnforce {
		t.Error("0.95 and never should enforce")
	}
	if ev(20, 19, "prefer small commits", "", "") != StatusActive {
		t.Error("soft rule must not enforce")
	}
	if ev(20, 17, "never push", "", StatusActive) != StatusSuggestion {
		t.Error("0.85 should demote")
	}
	if ev(0, 0, "never push", UserEnforce, "") != StatusEnforce || ev(20, 20, "never push", UserOff, "") != StatusDisabled {
		t.Error("user state wins")
	}
	if s, _ := p.Decide(Evidence{Labelled: 20, True: 20, Matches: 500, MatchRate: 0.5, RuleText: "never x"}); s != StatusSuggestion {
		t.Error("topic detector must stay a suggestion")
	}
}

func TestStoreLiveDemotion(t *testing.T) {
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := DefaultPolicy()
	sp := Spec{Shape: ShapeForbid, Action: `\bgit\s+push\b`}
	r := Row{ID: ID("note:a.md", sp), Target: "note:a.md", RuleText: "Never git push.", Spec: sp, Matches: 40, MatchRate: 0.01, Labelled: 10, True: 10}
	if err := st.ReplaceTarget("note:a.md", []Row{r}, p); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Get(r.ID[:6])
	if got.Status != StatusEnforce {
		t.Fatalf("status %s (%s)", got.Status, got.Reason)
	}
	live := &Live{Store: st}
	hits := live.Check("sess-1", "Bash", "git push origin main", "/x", "claude-code")
	if len(hits) != 1 || !hits[0].Enforce {
		t.Fatalf("live hits %+v", hits)
	}
	var last Row
	demoted := false
	for i := 0; i < 6; i++ {
		ids := live.Observe("sess-1", "Bash", "git push", "/x", "")
		if len(ids) != 1 {
			t.Fatal("no firing")
		}
		last, err = st.LabelFiring(ids[0], false, p)
		if err != nil {
			t.Fatal(err)
		}
		demoted = demoted || strings.Contains(last.Reason, "demoted")
	}
	if last.Status != StatusSuggestion || !demoted {
		t.Fatalf("not demoted: %s (%s) p=%.2f", last.Status, last.Reason, last.Precision)
	}
	live.Invalidate()
	if len(live.Check("s", "Bash", "git push", "", "")) != 0 {
		t.Fatal("demoted check still fires")
	}
	if _, err := st.SetUser(r.ID, UserEnable, p); err != nil {
		t.Fatal(err)
	}
	live.Invalidate()
	if len(live.Check("s", "Bash", "git push", "", "")) != 1 {
		t.Fatal("user-enabled check should fire")
	}
	_ = filepath.Join
}

func TestLiveRequireBeforeHistory(t *testing.T) {
	st, _ := Open(t.TempDir())
	defer st.Close()
	sp := Spec{Shape: ShapeRequireBefore, Action: `\bgit\s+push\b`, Before: `\bgo\s+test\b`}
	r := Row{ID: ID("note:b.md", sp), Target: "note:b.md", RuleText: "Run tests before push", Spec: sp, Matches: 9, Labelled: 9, True: 9, MatchRate: 0.01}
	st.ReplaceTarget("note:b.md", []Row{r}, DefaultPolicy())
	live := &Live{Store: st}
	if len(live.Check("s1", "Bash", "git push", "", "")) != 1 {
		t.Fatal("should flag push with no tests")
	}
	live.Observe("s1", "Bash", "go test ./...", "", "")
	if len(live.Check("s1", "Bash", "git push", "", "")) != 0 {
		t.Fatal("tests ran first; should pass")
	}
	if len(live.Check("s2", "Bash", "git push", "", "")) != 1 {
		t.Fatal("another session has its own history")
	}
}

func TestParseLLMTolerant(t *testing.T) {
	got, err := ParseLLM("```json\n{\"checks\":[{\"shape\":\"forbid\",\"action\":\"\\\\bpkill\\\\s+tmux\\\\b\",\"scope_cwd\":\".*\"}]}\n```")
	if err != nil || len(got) != 1 || got[0].Scope.Cwd != "" {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestMergeTreatsDefaultToolAsBash(t *testing.T) {
	a := Candidate{Spec: Spec{Shape: ShapeForbid, Action: `\bpgrep\s+-f\b`}, Source: "deterministic"}
	b := Candidate{Spec: Spec{Shape: ShapeForbid, Action: `\bpgrep\s+-f\b`, Tools: []string{"Bash"}}, Source: "llm"}
	if got := Merge([]Candidate{a}, []Candidate{b}); len(got) != 1 || got[0].Source != "deterministic" {
		t.Fatalf("%+v", got)
	}
}
