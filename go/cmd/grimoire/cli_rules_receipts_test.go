package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

func TestReceiptsSplitRefusedWhileHoldoutRuns(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	if _, err := parseReceiptsArgs([]string{"--split-injected"}, now); err == nil || !strings.Contains(err.Error(), "--before 2026-10-11") {
		t.Fatalf("split without cutoff must be refused with the cutoff hint, got %v", err)
	}
	if _, err := parseReceiptsArgs([]string{"--split-injected", "--before", "2026-10-12"}, now); err == nil {
		t.Fatal("cutoff after the holdout start must be refused")
	}
	o, err := parseReceiptsArgs([]string{"--split-injected", "--before", "2026-10-11"}, now)
	if err != nil || !o.split || o.before.Format("2006-01-02") != "2026-10-11" {
		t.Fatalf("cutoff at holdout start should pass the guard: %+v %v", o, err)
	}
	after := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if o, err := parseReceiptsArgs([]string{"--split-injected"}, after); err != nil || !o.split {
		t.Fatalf("after the holdout the guard should pass: %+v %v", o, err)
	}
}

func TestReceiptsFlagParsing(t *testing.T) {
	now := time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)
	o, err := parseReceiptsArgs([]string{"--since", "30d", "--agent", "claude-code", "--status", "active", "--json"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if o.since != 30*24*time.Hour || o.agent != "claude-code" || o.status != rulecheck.StatusActive || !o.asJSON || o.split {
		t.Fatalf("parsed: %+v", o)
	}
	if _, err := parseReceiptsArgs([]string{"--status", "maybe"}, now); err == nil {
		t.Fatal("unknown status accepted")
	}
	if _, err := parseReceiptsArgs([]string{"--since", "soon"}, now); err == nil {
		t.Fatal("bad --since accepted")
	}
	if _, err := parseReceiptsArgs([]string{"--before", "11/10/2026"}, now); err == nil {
		t.Fatal("bad --before accepted")
	}
}

func TestReceiptsTextRendersRateAndInterval(t *testing.T) {
	rate, lo, hi, prec := 2.0/3.0, 0.2077, 0.9386, 0.8
	rep := rulecheck.Report{Sessions: 4, Calls: 6, Checks: []rulecheck.Receipt{{
		ID: "p1", Target: "note:Agent Memory/rule_tests.md", Shape: "require_before", Describe: "/git push/ needs earlier /go test/",
		Status: "active", Labelled: 5, Precision: &prec, Sessions: 4, Actions: 3, Violations: 2, Compliant: 1,
		Rate: &rate, CILow: &lo, CIHigh: &hi,
	}, {ID: "r1", Status: "suggestion", Shape: "forbid", Target: "note:x.md", Describe: "forbid /rm -rf/"}},
		Days: []rulecheck.DayTotal{{Day: "2026-10-01", Sessions: 2, Calls: 3, Violations: 1}}}
	out := ReceiptsText(rep)
	for _, want := range []string{"4 sessions, 6 tool calls, 2 checks", "p1", "66.7% [20.8-93.9]", "0.80 n=5", "rule_tests", "n/a", "2026-10-01"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
