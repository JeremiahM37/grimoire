package api

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

const denyRuleText = "Never run git push on the kestrel repository without being asked"

// seedPushRule stores a memory and makes its compiled check live. A hard rule
// ("Never") at full precision earns enforce; a soft rule earns active.
func seedPushRule(t *testing.T, h http.Handler, srv *Server, text string, human bool) {
	t.Helper()
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": text, "human": human})
	target := "fact:" + fact["id"].(string)
	st, _ := srv.rules()
	sp := rulecheck.Spec{Shape: rulecheck.ShapeRequireBefore, Action: `\bgit\s+push\b`, Before: `\bgo\s+test\b`}
	row := rulecheck.Row{ID: rulecheck.ID(target, sp), Target: target, RuleText: text, Spec: sp,
		Matches: 30, MatchRate: 0.01, Labelled: 12, True: 12}
	if err := st.ReplaceTarget(target, []rulecheck.Row{row}, rulecheck.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
}

// pushPermission asks the action stage about a push in session and returns the
// permission decision it carries, or nil.
func pushPermission(t *testing.T, h http.Handler, session string) map[string]any {
	t.Helper()
	out := ctxJSON(t, h, "Bash git push origin main", "stage", "action", "min_rel", "0.9", "session", session)
	perm, _ := out["permission"].(map[string]any)
	return perm
}

func TestDenyOnceOffKeepsAsking(t *testing.T) {
	restore := exp.Set()
	defer restore()
	srv, h := testServer(t)
	seedPushRule(t, h, srv, denyRuleText, true)
	for i := 0; i < 2; i++ {
		perm := pushPermission(t, h, "denyonce-off-0001")
		if perm == nil || perm["decision"] != "ask" {
			t.Fatalf("flag off, firing %d: want ask, got %v", i+1, perm)
		}
		if _, has := perm["once"]; has {
			t.Fatalf("flag off must not add a once field: %v", perm)
		}
	}
}

func TestDenyOnceDeniesFirstFiringThenAsks(t *testing.T) {
	restore := exp.Set(denyOnceFlag)
	defer restore()
	srv, h := testServer(t)
	seedPushRule(t, h, srv, denyRuleText, true)

	perm := pushPermission(t, h, "denyonce-on-0001")
	if perm["decision"] != "deny" || perm["once"] != true {
		t.Fatalf("first firing should deny once: %v", perm)
	}
	reason, _ := perm["reason"].(string)
	if reason == "" || containsNewline(reason) || !strings.Contains(reason, denyRuleText) {
		t.Fatalf("deny reason should be one line with the rule text: %q", reason)
	}
	perm = pushPermission(t, h, "denyonce-on-0001")
	if perm["decision"] != "ask" {
		t.Fatalf("second firing in the same session should ask: %v", perm)
	}
	if _, has := perm["once"]; has {
		t.Fatalf("an ask must not carry once: %v", perm)
	}
	// Another session has its own first firing.
	if perm := pushPermission(t, h, "denyonce-on-0002"); perm["decision"] != "deny" {
		t.Fatalf("a new session should deny once: %v", perm)
	}
}

func TestDenyOnceNeverDeniesAnAgentRule(t *testing.T) {
	restore := exp.Set(denyOnceFlag)
	defer restore()
	srv, h := testServer(t)
	seedPushRule(t, h, srv, denyRuleText, false)
	for i := 0; i < 2; i++ {
		perm := pushPermission(t, h, "denyonce-agent-0001")
		if perm == nil || perm["decision"] != "ask" {
			t.Fatalf("agent-authored rule, firing %d: want ask, got %v", i+1, perm)
		}
	}
}

func TestDenyOnceNeverDeniesBelowEnforce(t *testing.T) {
	restore := exp.Set(denyOnceFlag)
	defer restore()
	srv, h := testServer(t)
	// No "never"/"always" wording, so the check earns active, not enforce.
	seedPushRule(t, h, srv, "Avoid git push on the kestrel repository unless the user asks", true)
	if perm := pushPermission(t, h, "denyonce-active-0001"); perm != nil && perm["decision"] != "ask" {
		t.Fatalf("an active (reminder-only) check must never deny: %v", perm)
	}
}

func TestDenyOncePreDecisionNeverConsumesTheDeny(t *testing.T) {
	restore := exp.Set(denyOnceFlag)
	defer restore()
	srv, h := testServer(t)
	seedPushRule(t, h, srv, denyRuleText, true)
	// The outcome hook's pre-decision asks and must leave the deny unspent.
	pre := postOutcome(t, h, map[string]any{"session": "denyonce-pre-0001", "tool": "Bash",
		"target": "git push", "pre": true})
	if p, _ := pre["permission"].(map[string]any); p == nil || p["decision"] != "ask" {
		t.Fatalf("pre-decision should ask: %v", pre)
	}
	if perm := pushPermission(t, h, "denyonce-pre-0001"); perm["decision"] != "deny" {
		t.Fatalf("the action-stage firing should still be the first deny: %v", perm)
	}
}

func containsNewline(s string) bool { return strings.ContainsAny(s, "\r\n") }

func TestDenyLedgerIsBoundedAndForgetsTheOldest(t *testing.T) {
	var l denyLedger
	if !l.claim("s", "c0") || l.claim("s", "c0") {
		t.Fatal("a pair is claimable once")
	}
	for i := 1; i <= denyLedgerMax; i++ {
		l.claim("s", fmt.Sprint("c", i))
	}
	if len(l.seen) != denyLedgerMax || len(l.order) != denyLedgerMax {
		t.Fatalf("ledger should hold %d pairs, has %d/%d", denyLedgerMax, len(l.seen), len(l.order))
	}
	if !l.claim("s", "c0") {
		t.Fatal("the oldest pair should have been evicted")
	}
	if l.claim("s", fmt.Sprint("c", denyLedgerMax)) {
		t.Fatal("a recent pair must still be remembered")
	}
}
