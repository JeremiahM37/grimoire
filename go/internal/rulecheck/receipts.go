package rulecheck

import (
	"fmt"
	"sort"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// The randomised memory holdout (benchmarks/holdout/PREREG.md in the Grimoire
// repo). Outcomes split by injection arm must not be read before HoldoutEnd.
var (
	HoldoutStart = time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	HoldoutEnd   = time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
)

// Receipt is one stored check's adherence over a corpus: how often the calls it
// covers broke the rule, not whether the agent mentioned the memory.
type Receipt struct {
	ID         string   `json:"id"`
	Target     string   `json:"target"`
	Shape      string   `json:"shape"`
	Describe   string   `json:"describe"`
	Status     string   `json:"status"`
	Labelled   int      `json:"labelled"`
	Precision  *float64 `json:"precision,omitempty"`      // labelled precision; nil when nothing is labelled
	Sessions   int      `json:"sessions"`                 // sessions with at least one call in the check's tools and scope
	Actions    int      `json:"actions"`                  // calls that were the rule's subject
	Violations int      `json:"violations"`               // actions that broke the rule
	Compliant  int      `json:"compliant_actions"`        // actions that kept it
	Rate       *float64 `json:"violation_rate,omitempty"` // violations / actions; nil when there are no actions
	CILow      *float64 `json:"ci95_low,omitempty"`
	CIHigh     *float64 `json:"ci95_high,omitempty"`
}

// DayTotal is the corpus summed by the day each session started (UTC).
type DayTotal struct {
	Day        string `json:"day"`
	Sessions   int    `json:"sessions"`
	Calls      int    `json:"tool_calls"`
	Violations int    `json:"violations"` // summed over the checks, so one call can count once per check
}

// Report is the receipts for every check in scope.
type Report struct {
	Checks   []Receipt  `json:"checks"`
	Days     []DayTotal `json:"days"`
	Sessions int        `json:"sessions"`
	Calls    int        `json:"tool_calls"`
	Skipped  []string   `json:"skipped,omitempty"` // stored checks whose spec no longer compiles
}

// Receipts walks the corpus once, in session order, and counts for each stored
// check the sessions it applied to, its actions, and how many of those actions
// broke the rule. status "" keeps every status. The history a require_before
// check reads is built the same way as in Run: every tool call of the session,
// oldest first, capped at MaxHistory.
func Receipts(rows []Row, status string, corpus Corpus) Report {
	var rep Report
	var cs []*Compiled
	var keep []Row
	for _, r := range rows {
		if status != "" && r.Status != status {
			continue
		}
		c, err := r.Spec.Compile()
		if err != nil {
			rep.Skipped = append(rep.Skipped, r.ID+": "+err.Error())
			continue
		}
		cs = append(cs, c)
		keep = append(keep, r)
	}
	n := len(cs)
	sessions := make([]int, n)
	actions := make([]int, n)
	violations := make([]int, n)
	compliant := make([]int, n)
	days := map[string]*DayTotal{}

	for _, s := range corpus.Sessions {
		applied := make([]bool, n)
		day := dayOf(s.Start)
		dt := days[day]
		if dt == nil {
			dt = &DayTotal{Day: day}
			days[day] = dt
		}
		dt.Sessions++
		var history []string
		for _, t := range s.Turns {
			if t.Role == transcript.RoleUser {
				continue
			}
			dt.Calls++
			for ci, c := range cs {
				if !c.AppliesTo(t.Tool, s.Cwd, s.Agent) {
					continue
				}
				applied[ci] = true
				if !c.IsAction(t.ToolTarget) {
					continue
				}
				actions[ci]++
				if c.Violates(t.ToolTarget, history) {
					violations[ci]++
					dt.Violations++
				} else {
					compliant[ci]++
				}
			}
			if len(history) >= MaxHistory {
				history = history[1:]
			}
			history = append(history, t.ToolTarget)
		}
		for ci := range cs {
			if applied[ci] {
				sessions[ci]++
			}
		}
	}

	for ci := range cs {
		e := keep[ci].Evidence("")
		rc := Receipt{ID: keep[ci].ID, Target: keep[ci].Target, Shape: keep[ci].Spec.Shape,
			Describe: keep[ci].Spec.Describe(), Status: keep[ci].Status, Labelled: e.Labelled,
			Sessions: sessions[ci], Actions: actions[ci], Violations: violations[ci], Compliant: compliant[ci]}
		if e.Labelled > 0 {
			p := e.Precision()
			rc.Precision = &p
		}
		if a := actions[ci]; a > 0 {
			rate := float64(violations[ci]) / float64(a)
			lo, hi := Wilson(violations[ci], a)
			rc.Rate, rc.CILow, rc.CIHigh = &rate, &lo, &hi
		}
		rep.Checks = append(rep.Checks, rc)
	}
	sort.Slice(rep.Checks, func(i, j int) bool {
		a, b := rep.Checks[i], rep.Checks[j]
		if a.Violations != b.Violations {
			return a.Violations > b.Violations
		}
		if a.Actions != b.Actions {
			return a.Actions > b.Actions
		}
		return a.ID < b.ID
	})
	for _, d := range days {
		rep.Days = append(rep.Days, *d)
	}
	sort.Slice(rep.Days, func(i, j int) bool { return rep.Days[i].Day < rep.Days[j].Day })
	rep.Sessions = len(corpus.Sessions)
	rep.Calls = corpus.Calls
	return rep
}

func dayOf(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format("2006-01-02")
}

// BeforeCorpus keeps the sessions that started strictly before the instant.
// Sessions with no known start are dropped, since they cannot be placed.
func BeforeCorpus(c Corpus, before time.Time) Corpus {
	var out []transcript.Session
	n := 0
	for _, s := range c.Sessions {
		if s.Start.IsZero() || !s.Start.Before(before) {
			continue
		}
		out = append(out, s)
		for _, t := range s.Turns {
			if t.Role == transcript.RoleTool {
				n++
			}
		}
	}
	return Corpus{Sessions: out, Calls: n}
}

// SplitGuard decides whether outcomes may be split by injection arm at now.
// While the holdout runs (HoldoutStart to HoldoutEnd) the split is refused,
// unless the history is restricted to sessions that started before the holdout
// did (before is then at or before HoldoutStart). A zero before means no
// restriction. Once HoldoutEnd has passed, the split is allowed.
func SplitGuard(now, before time.Time) error {
	if !now.Before(HoldoutEnd) {
		return nil
	}
	if before.IsZero() {
		return fmt.Errorf("refusing --split-injected: the memory holdout runs %s to %s and its arms must not be compared before %s. "+
			"To split only history from before the holdout, pass --before %s",
			HoldoutStart.Format("2006-01-02"), HoldoutEnd.AddDate(0, 0, -1).Format("2006-01-02"),
			HoldoutEnd.Format("2006-01-02"), HoldoutStart.Format("2006-01-02"))
	}
	if before.After(HoldoutStart) {
		return fmt.Errorf("refusing --split-injected: --before %s is after the holdout started on %s; "+
			"use --before %s or earlier",
			before.Format("2006-01-02"), HoldoutStart.Format("2006-01-02"), HoldoutStart.Format("2006-01-02"))
	}
	return nil
}
