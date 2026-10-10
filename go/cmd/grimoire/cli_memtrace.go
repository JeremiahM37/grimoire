package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

type causalOut struct {
	Label     string                `json:"label"`
	Status    string                `json:"status"`
	Rate      float64               `json:"holdout_rate"`
	Treated   int                   `json:"treated"`
	Withheld  int                   `json:"withheld"`
	MinArm    int                   `json:"min_per_arm"`
	Estimator string                `json:"estimator"`
	Estimate  *utilization.Estimate `json:"estimate"`
	Note      string                `json:"note"`
}

func pct(iv adherence.Interval) string {
	if iv.N == 0 {
		return "n/a (n=0)"
	}
	return fmt.Sprintf("%.0f%% [%.0f-%.0f%%] (%d/%d)", 100*iv.Rate, 100*iv.Lo, 100*iv.Hi, iv.K, iv.N)
}

func benefitText(b adherence.Benefit) string {
	if b.Matched == 0 {
		return fmt.Sprintf("associated: no comparison (%d influenced actions; %s)", b.Influenced, b.Note)
	}
	return fmt.Sprintf("associated: bad outcomes %.0f%% on %d influenced actions vs %.0f%% on %d comparable ones; difference %+.1f pts [%+.1f to %+.1f] over %d strata",
		100*b.BadInfluence.Rate, b.Matched, 100*b.BadMatched, b.Comparison, 100*b.Diff, 100*b.Lo, 100*b.Hi, b.Strata)
}

func causalText(c causalOut) string {
	if c.Estimate == nil {
		s := fmt.Sprintf("%s (shown %d, withheld %d, need %d per arm)", c.Status, c.Treated, c.Withheld, c.MinArm)
		if c.Note != "" {
			s += ": " + c.Note
		}
		return s
	}
	e := c.Estimate
	return fmt.Sprintf("caused (%s): bad-outcome share %+.1f pts [%+.1f to %+.1f], n=%d (shown %d, withheld %d)",
		e.Method, 100*e.Effect, 100*e.Lo, 100*e.Hi, e.N, e.Treated, e.Control)
}

func remindersText(r adherence.ReminderStats) string {
	if r.Reminders == 0 {
		return "none"
	}
	s := fmt.Sprintf("%d shown for a pending action: %d ran unchanged, %d changed after the reminder, %d never ran, %d open; change rate %s",
		r.Reminders, r.Unchanged, r.Changed, r.Abandoned, r.Open, pct(r.Change))
	if r.Ask > 0 {
		s += fmt.Sprintf("; enforce-ask %d (ran %d, not run %d)", r.Ask, r.AskRan, r.AskNot)
	}
	return s
}

// memoryTraceCmd shows the utilization trace: one memory with --target, else
// the roll-up by kind.
func memoryTraceCmd(e *env, args []string) int {
	q := url.Values{}
	if v := flagOr(args, "--days", ""); v != "" {
		q.Set("days", v)
	}
	target := flagOr(args, "--target", "")
	path := "/api/memory/trace/summary"
	if target != "" {
		q.Set("target", target)
		path = "/api/memory/trace"
	}
	status, raw := e.call("GET", path+"?"+q.Encode())
	if status != http.StatusOK {
		return fail("trace failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	if target != "" {
		var out struct {
			Days   int            `json:"days"`
			Card   adherence.Card `json:"card"`
			Causal causalOut      `json:"causal"`
		}
		if err := json.Unmarshal([]byte(raw), &out); err != nil {
			return fail("%v", err)
		}
		c := out.Card
		fmt.Printf("%s  (%s, last %d days)\n", c.Target, kindOrUnknown(c.Kind), out.Days)
		fmt.Printf("  exposures        %d shown, %d withheld by the holdout\n", c.Exposures, c.Withheld)
		fmt.Printf("  uptake           %s   (tag cited or fingerprint matched)\n", pct(c.Uptake))
		fmt.Printf("  influence        %s   (linked to a concrete action)\n", pct(c.Influence))
		fmt.Printf("  link evidence    %s\n", countsText(c.Evidence))
		fmt.Printf("  linked actions   %d, %d observed: %d failed, %d tests failed, %d re-edited, %d reverted, %d thrash, %d denied, %d corrected\n",
			c.Linked.Actions, c.Linked.Observed, c.Linked.Failed, c.Linked.TestsFail, c.Linked.Reedited, c.Linked.Reverted, c.Linked.Thrash, c.Linked.Denied, c.Linked.Corrected)
		fmt.Printf("  after reminder   %s\n", remindersText(c.Reminders))
		fmt.Printf("  benefit          %s\n", benefitText(c.Benefit))
		fmt.Printf("  causal           %s\n", causalText(out.Causal))
		return 0
	}
	var out struct {
		Days        int     `json:"days"`
		HoldoutRate float64 `json:"holdout_rate"`
		Kinds       []struct {
			adherence.KindSummary
			Causal causalOut `json:"causal"`
		} `json:"kinds"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("memory utilization, last %d days (holdout rate %.2f)\n", out.Days, out.HoldoutRate)
	for _, k := range out.Kinds {
		name := k.Kind
		if name == "" {
			name = "all"
		}
		fmt.Printf("\n%s: %d memories, %d exposures, %d withheld\n", name, k.Memories, k.Exposures, k.Withheld)
		fmt.Printf("  uptake    %s\n  influence %s\n  reminders %s\n  benefit   %s\n  causal    %s\n",
			pct(k.Uptake), pct(k.Influence), remindersText(k.Reminders), benefitText(k.Benefit), causalText(k.Causal))
	}
	return 0
}

func kindOrUnknown(s string) string {
	if s == "" {
		return "kind unknown"
	}
	return s
}

func countsText(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	var parts []string
	for _, k := range []string{adherence.EvTag, adherence.EvFingerprint, adherence.EvCheck, adherence.EvChanged} {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
		}
	}
	return strings.Join(parts, ", ")
}
