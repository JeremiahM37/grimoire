package rulecheck

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

func rtool(target string) transcript.Turn {
	return transcript.Turn{Role: transcript.RoleTool, Tool: "Bash", ToolTarget: target}
}

func rsession(id string, start time.Time, targets ...string) transcript.Session {
	s := transcript.Session{Agent: "claude-code", ID: id, Cwd: "/home/a/repo", Start: start}
	for _, t := range targets {
		s.Turns = append(s.Turns, rtool(t))
	}
	return s
}

// receiptCorpus: A runs tests then pushes (compliant); B pushes with no tests
// (violation); C only lists files (applies, no action); D pushes with no tests
// and runs tests afterwards (violation, the later tests do not help).
func receiptCorpus() Corpus {
	return BuildCorpus([]transcript.Session{
		rsession("A", time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC), "go test ./...", "git push origin a"),
		rsession("B", time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC), "git push origin b"),
		rsession("C", time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), "ls"),
		rsession("D", time.Date(2026, 10, 2, 10, 0, 0, 0, time.UTC), "git push origin c", "go test ./..."),
	})
}

func pushRule(id, status string) Row {
	sp := Spec{Shape: ShapeRequireBefore, Action: `\bgit\s+push\b`, Before: `\bgo\s+test\b`}
	return Row{ID: id, Target: "note:Agent Memory/rule_tests.md", RuleText: "Run tests before push",
		Spec: sp, Status: status, Labelled: 5, True: 4}
}

func rmRule(id string) Row {
	return Row{ID: id, Target: "note:Agent Memory/rule_rm.md", Spec: Spec{Shape: ShapeForbid, Action: `\brm\s+-rf\b`}, Status: StatusSuggestion}
}

func TestReceiptsCountsSessionsActionsViolations(t *testing.T) {
	rep := Receipts([]Row{pushRule("p1", StatusActive), rmRule("r1")}, "", receiptCorpus())
	if rep.Sessions != 4 || rep.Calls != 6 {
		t.Fatalf("corpus totals: sessions %d calls %d", rep.Sessions, rep.Calls)
	}
	if len(rep.Checks) != 2 || rep.Checks[0].ID != "p1" {
		t.Fatalf("want p1 first (most violations): %+v", rep.Checks)
	}
	p := rep.Checks[0]
	if p.Sessions != 4 || p.Actions != 3 || p.Violations != 2 || p.Compliant != 1 {
		t.Fatalf("push check counts: %+v", p)
	}
	if p.Rate == nil || math.Abs(*p.Rate-2.0/3.0) > 1e-9 {
		t.Fatalf("violation rate: %+v", p.Rate)
	}
	if p.Precision == nil || math.Abs(*p.Precision-0.8) > 1e-9 || p.Labelled != 5 {
		t.Fatalf("labelled precision: %+v", p.Precision)
	}
	r := rep.Checks[1]
	if r.Sessions != 4 || r.Actions != 0 || r.Violations != 0 || r.Rate != nil || r.CILow != nil || r.Precision != nil {
		t.Fatalf("rm check with no actions should carry no rate: %+v", r)
	}
}

func TestReceiptsSessionDayTotals(t *testing.T) {
	rep := Receipts([]Row{pushRule("p1", StatusActive)}, "", receiptCorpus())
	if len(rep.Days) != 2 {
		t.Fatalf("days: %+v", rep.Days)
	}
	d1, d2 := rep.Days[0], rep.Days[1]
	if d1.Day != "2026-10-01" || d1.Sessions != 2 || d1.Calls != 3 || d1.Violations != 1 {
		t.Fatalf("day 1: %+v", d1)
	}
	if d2.Day != "2026-10-02" || d2.Sessions != 2 || d2.Calls != 3 || d2.Violations != 1 {
		t.Fatalf("day 2: %+v", d2)
	}
}

func TestReceiptsStatusFilter(t *testing.T) {
	rows := []Row{pushRule("p1", StatusActive), rmRule("r1")}
	rep := Receipts(rows, StatusActive, receiptCorpus())
	if len(rep.Checks) != 1 || rep.Checks[0].ID != "p1" {
		t.Fatalf("status filter: %+v", rep.Checks)
	}
	if rep := Receipts(rows, StatusEnforce, receiptCorpus()); len(rep.Checks) != 0 {
		t.Fatalf("enforce should match none: %+v", rep.Checks)
	}
}

func TestReceiptsSkipsUncompilable(t *testing.T) {
	bad := Row{ID: "bad", Spec: Spec{Shape: ShapeForbid, Action: ".*"}}
	rep := Receipts([]Row{bad, pushRule("p1", StatusActive)}, "", receiptCorpus())
	if len(rep.Checks) != 1 || len(rep.Skipped) != 1 || !strings.HasPrefix(rep.Skipped[0], "bad: ") {
		t.Fatalf("skip: checks %+v skipped %v", rep.Checks, rep.Skipped)
	}
}

func TestReceiptsWilsonInterval(t *testing.T) {
	rep := Receipts([]Row{pushRule("p1", StatusActive)}, "", receiptCorpus())
	p := rep.Checks[0]
	// Wilson 95% for 2 violations in 3 actions.
	if p.CILow == nil || p.CIHigh == nil || math.Abs(*p.CILow-0.2077) > 0.002 || math.Abs(*p.CIHigh-0.9386) > 0.002 {
		t.Fatalf("interval: %v %v", p.CILow, p.CIHigh)
	}
	if lo, hi := Wilson(2, 3); lo != *p.CILow || hi != *p.CIHigh {
		t.Fatalf("report interval differs from Wilson(2,3)")
	}
}

func TestBeforeCorpusCutsByStart(t *testing.T) {
	c := receiptCorpus()
	cut := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	got := BeforeCorpus(c, cut)
	if len(got.Sessions) != 2 || got.Sessions[0].ID != "A" || got.Sessions[1].ID != "B" {
		t.Fatalf("before cutoff kept %+v", got.Sessions)
	}
	if got.Calls != 3 {
		t.Fatalf("calls recount: %d", got.Calls)
	}
	// A session with no known start cannot be placed before the cutoff.
	undated := BuildCorpus([]transcript.Session{rsession("U", time.Time{}, "git push x")})
	if n := len(BeforeCorpus(undated, cut).Sessions); n != 0 {
		t.Fatalf("undated session kept: %d", n)
	}
}

func TestSplitGuardRefusesDuringHoldout(t *testing.T) {
	at := func(y int, m time.Month, d, h int) time.Time { return time.Date(y, m, d, h, 0, 0, 0, time.UTC) }
	cut := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name   string
		now    time.Time
		before time.Time
		refuse bool
	}{
		{"today, no cutoff", at(2026, 10, 14, 9), time.Time{}, true},
		{"holdout start, no cutoff", at(2026, 10, 11, 0), time.Time{}, true},
		{"last holdout day, no cutoff", at(2026, 11, 8, 23), time.Time{}, true},
		{"cutoff at holdout start", at(2026, 10, 14, 9), cut(2026, 10, 11), false},
		{"cutoff before holdout", at(2026, 10, 14, 9), cut(2026, 10, 1), false},
		{"cutoff after holdout start", at(2026, 10, 14, 9), cut(2026, 10, 12), true},
		{"holdout over", at(2026, 11, 9, 0), time.Time{}, false},
		{"holdout over, late cutoff", at(2026, 11, 9, 0), cut(2026, 11, 1), false},
	}
	for _, c := range cases {
		err := SplitGuard(c.now, c.before)
		if (err != nil) != c.refuse {
			t.Errorf("%s: refuse=%v err=%v", c.name, c.refuse, err)
		}
		if err != nil && !strings.Contains(err.Error(), "refusing --split-injected") {
			t.Errorf("%s: unclear message: %v", c.name, err)
		}
	}
}
