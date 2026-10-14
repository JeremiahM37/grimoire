package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/api"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

const rulesUsage = `usage:
  grimoire rules compile [--dir STORE] [--prefix "Agent Memory/"] [--no-llm]
  grimoire rules backtest [--since 90d] [--agent NAME] [--budget 1500] [--no-label]
  grimoire rules list [--status suggestion|active|enforce|disabled] [--summary] [--json]
  grimoire rules show ID
  grimoire rules enable ID [--enforce]
  grimoire rules disable ID
  grimoire rules label FIRING_ID violation|false-alarm
  grimoire rules receipts [--status S] [--since 90d] [--agent NAME] [--before YYYY-MM-DD] [--json]

Rules (memories of kind rule) are compiled into checks over tool calls, each check
is measured against your transcript history, and only precise ones act.
receipts reports how often each check's calls broke its rule (see docs/MEMORY_RULES.md).
See docs/MEMORY_RULES.md.`

func cmdRules(args []string) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println(rulesUsage)
		return 0
	}
	if args[0] == "receipts" {
		return rulesReceipts(args[1:], time.Now())
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	rest := args[1:]
	switch args[0] {
	case "compile":
		body := map[string]any{"dir": flagOr(rest, "--dir", ""), "prefix": flagOr(rest, "--prefix", "")}
		if hasFlag(rest, "--no-llm") {
			body["llm"] = false
		}
		return rulesPost(e, "/api/memory/rules/compile", body)
	case "backtest":
		body := map[string]any{"since": flagOr(rest, "--since", ""), "agent": flagOr(rest, "--agent", "")}
		if hasFlag(rest, "--no-label") {
			body["label"] = false
		}
		if v := flagOr(rest, "--budget", ""); v != "" {
			var n int
			fmt.Sscan(v, &n)
			body["budget"] = n
		}
		return rulesPost(e, "/api/memory/rules/backtest", body)
	case "enable", "disable":
		ids := positional(rest)
		if len(ids) != 1 {
			return fail("usage: grimoire rules %s ID", args[0])
		}
		body := map[string]any{"id": ids[0]}
		if args[0] == "enable" && hasFlag(rest, "--enforce") {
			body["enforce"] = true
		}
		return rulesPost(e, "/api/memory/rules/"+args[0], body)
	case "label":
		ids := positional(rest)
		if len(ids) != 2 {
			return fail("usage: grimoire rules label FIRING_ID violation|false-alarm")
		}
		var n int64
		fmt.Sscan(ids[0], &n)
		return rulesPost(e, "/api/memory/rules/label", map[string]any{"firing": n, "violation": ids[1] == "violation"})
	case "list":
		return rulesList(e, rest)
	case "show":
		ids := positional(rest)
		if len(ids) != 1 {
			return fail("usage: grimoire rules show ID")
		}
		return rulesShow(e, ids[0])
	}
	return fail("unknown rules command %q\n\n%s", args[0], rulesUsage)
}

func rulesPost(e *env, path string, body any) int {
	status, raw := e.callBody("POST", path, body)
	if status != http.StatusOK {
		return fail("%s failed: %s", path, raw)
	}
	var pretty map[string]any
	if json.Unmarshal([]byte(raw), &pretty) == nil {
		b, _ := json.MarshalIndent(pretty, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Println(raw)
	return 0
}

func rulesList(e *env, args []string) int {
	q := url.Values{}
	if v := flagOr(args, "--status", ""); v != "" {
		q.Set("status", v)
	}
	status, raw := e.call("GET", "/api/memory/rules?"+q.Encode())
	if status != http.StatusOK {
		return fail("list failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	var out struct {
		Checks []rulecheck.Row `json:"checks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	fmt.Print(RulesText(out.Checks, hasFlag(args, "--summary")))
	return 0
}

// RulesText renders checks for a terminal.
func RulesText(rows []rulecheck.Row, summary bool) string {
	var b strings.Builder
	counts := map[string]int{}
	for _, r := range rows {
		counts[r.Status]++
	}
	fmt.Fprintf(&b, "%d checks: %d enforce, %d active, %d suggestions, %d disabled\n", len(rows),
		counts[rulecheck.StatusEnforce], counts[rulecheck.StatusActive], counts[rulecheck.StatusSuggestion], counts[rulecheck.StatusDisabled])
	if summary {
		return b.String()
	}
	for _, r := range rows {
		prec := "n/a"
		n := r.Labelled + r.LiveTrue + r.LiveFalse
		if n > 0 {
			prec = fmt.Sprintf("%.2f [%.2f-%.2f] n=%d", r.Precision, r.CILow, r.CIHigh, n)
		}
		fmt.Fprintf(&b, "%s  %-10s %-14s precision %s  matches %d in %d sessions\n    %s\n    %s (%s) — %s\n",
			r.ID, r.Status, r.Spec.Shape, prec, r.Matches, r.Sessions, strings.TrimSuffix(filepath.Base(strings.TrimPrefix(r.Target, "note:")), ".md"),
			r.Spec.Describe(), r.Source, r.Reason)
	}
	return b.String()
}

// rulesShow prints one check with the rule it came from and the labelled
// matches behind its precision.
func rulesShow(e *env, id string) int {
	status, raw := e.call("GET", "/api/memory/rules")
	if status != http.StatusOK {
		return fail("list failed: %s", raw)
	}
	var out struct {
		Checks []rulecheck.Row `json:"checks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	for _, r := range out.Checks {
		if !strings.HasPrefix(r.ID, id) {
			continue
		}
		fmt.Print(RulesText([]rulecheck.Row{r}, false))
		fmt.Printf("  rule: %s\n  scanned %d calls, %d were the rule's subject, %d flagged (%.2f%%), retold %d times\n",
			strings.Join(strings.Fields(r.RuleText), " ")[:min(300, len(strings.Join(strings.Fields(r.RuleText), " ")))], r.Scanned, r.Actions, r.Matches, 100*r.MatchRate, r.Retold)
		for _, m := range r.Sample {
			v := "false alarm"
			if m.True {
				v = "violation"
			}
			t := strings.Join(strings.Fields(m.Target), " ")
			fmt.Printf("  %-11s p=%.2f  %s\n", v, m.P, t[:min(160, len(t))])
		}
		return 0
	}
	return fail("no check %q", id)
}

// receiptsOpts is what `grimoire rules receipts` was asked for.
type receiptsOpts struct {
	status, agent string
	since         time.Duration
	before        time.Time
	split, asJSON bool
}

// parseReceiptsArgs reads the flags. A split by injection arm is refused by the
// holdout guard at now, before any store or transcript is opened.
func parseReceiptsArgs(args []string, now time.Time) (receiptsOpts, error) {
	var o receiptsOpts
	o.status = flagOr(args, "--status", "")
	switch o.status {
	case "", rulecheck.StatusEnforce, rulecheck.StatusActive, rulecheck.StatusSuggestion, rulecheck.StatusDisabled:
	default:
		return o, fmt.Errorf("--status %q: want enforce, active, suggestion or disabled", o.status)
	}
	d, err := api.ParseSince(flagOr(args, "--since", ""))
	if err != nil {
		return o, err
	}
	o.since = d
	o.agent = flagOr(args, "--agent", "")
	if v := flagOr(args, "--before", ""); v != "" {
		t, err := time.ParseInLocation("2006-01-02", v, time.UTC)
		if err != nil {
			return o, fmt.Errorf("--before %q: want YYYY-MM-DD", v)
		}
		o.before = t
	}
	o.split = hasFlag(args, "--split-injected")
	o.asJSON = hasFlag(args, "--json")
	if o.split {
		if err := rulecheck.SplitGuard(now, o.before); err != nil {
			return o, err
		}
	}
	return o, nil
}

// rulesReceipts: an adherence report from saved transcripts. For each stored
// check it counts how often the calls it covers broke the rule.
func rulesReceipts(args []string, now time.Time) int {
	o, err := parseReceiptsArgs(args, now)
	if err != nil {
		return fail("%v", err)
	}
	if o.split {
		return fail("not implemented: join to the injection trace (the holdout guard passed, the split itself is not built yet)")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	st, err := rulecheck.Open(filepath.Join(e.vault.Root, ".grimoire"))
	if err != nil {
		return fail("rules store: %v", err)
	}
	defer st.Close()
	rows, err := st.All()
	if err != nil {
		return fail("%v", err)
	}
	corpus, _, errs := rulecheck.LoadCorpus(rulecheck.HomeDir(), o.since, o.agent)
	for i, e := range errs {
		if i == 5 {
			break
		}
		fmt.Fprintln(os.Stderr, e)
	}
	if !o.before.IsZero() {
		corpus = rulecheck.BeforeCorpus(corpus, o.before)
	}
	rep := rulecheck.Receipts(rows, o.status, corpus)
	if o.asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(b))
		return 0
	}
	fmt.Print(ReceiptsText(rep))
	return 0
}

// ReceiptsText renders a receipts report for a terminal, worst first.
func ReceiptsText(rep rulecheck.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d sessions, %d tool calls, %d checks in scope\n", rep.Sessions, rep.Calls, len(rep.Checks))
	if len(rep.Checks) > 0 {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSTATUS\tSHAPE\tPRECISION\tSESSIONS\tACTIONS\tVIOLATIONS\tCOMPLIANT\tVIOLATION RATE\tRULE")
		for _, c := range rep.Checks {
			prec := "n/a"
			if c.Precision != nil {
				prec = fmt.Sprintf("%.2f n=%d", *c.Precision, c.Labelled)
			}
			rate := "n/a"
			if c.Rate != nil {
				rate = fmt.Sprintf("%.1f%% [%.1f-%.1f]", 100**c.Rate, 100**c.CILow, 100**c.CIHigh)
			}
			target := strings.TrimSuffix(filepath.Base(strings.TrimPrefix(c.Target, "note:")), ".md")
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s (%s)\n", c.ID, c.Status, c.Shape, prec,
				c.Sessions, c.Actions, c.Violations, c.Compliant, rate, c.Describe, target)
		}
		tw.Flush()
	}
	if len(rep.Days) > 0 {
		b.WriteString("\nper day (UTC)\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "DAY\tSESSIONS\tTOOL CALLS\tVIOLATIONS")
		for _, d := range rep.Days {
			fmt.Fprintf(tw, "%s\t%d\t%d\t%d\n", d.Day, d.Sessions, d.Calls, d.Violations)
		}
		tw.Flush()
	}
	for _, s := range rep.Skipped {
		fmt.Fprintf(&b, "skipped %s\n", s)
	}
	return b.String()
}
