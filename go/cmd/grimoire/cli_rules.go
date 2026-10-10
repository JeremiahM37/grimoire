package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

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

Rules (memories of kind rule) are compiled into checks over tool calls, each check
is measured against your transcript history, and only precise ones act.
See docs/MEMORY_RULES.md.`

func cmdRules(args []string) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println(rulesUsage)
		return 0
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
