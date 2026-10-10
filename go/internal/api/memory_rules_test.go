package api

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

func TestCompiledRuleChecksActAtActionStage(t *testing.T) {
	t.Parallel()
	srv, h := testServer(t)
	fact := remember(t, h, map[string]any{"topic": "kestrel", "text": pushRule})
	target := "fact:" + fact["id"].(string)
	st, _ := srv.rules()
	sp := rulecheck.Spec{Shape: rulecheck.ShapeRequireBefore, Action: `\bgit\s+push\b`, Before: `\bgo\s+test\b`}
	row := rulecheck.Row{ID: rulecheck.ID(target, sp), Target: target, RuleText: pushRule, Spec: sp,
		Matches: 30, MatchRate: 0.01, Labelled: 12, True: 12}
	if err := st.ReplaceTarget(target, []rulecheck.Row{row}, rulecheck.DefaultPolicy()); err != nil {
		t.Fatal(err)
	}
	// Active (precision 1.0, "Never" in the text) means enforce.
	out := ctxJSON(t, h, "Bash git push origin main", "stage", "action", "min_rel", "0.9", "session", "sess-rules-0001")
	perm, _ := out["permission"].(map[string]any)
	if perm["decision"] != "ask" {
		t.Fatalf("push without tests should ask: %v", out)
	}
	// Running the tests first (reported through the outcome hook) satisfies it.
	postOutcome(t, h, map[string]any{"session": "sess-rules-0001", "tool": "Bash", "target": "go test ./..."})
	out = ctxJSON(t, h, "Bash git push origin main", "stage", "action", "min_rel", "0.9", "session", "sess-rules-0001")
	if out["permission"] != nil {
		t.Fatalf("tests ran first, should pass: %v", out)
	}
	// A different session has its own history.
	pre := postOutcome(t, h, map[string]any{"session": "sess-rules-0002", "tool": "Bash", "target": "git push", "pre": true})
	if pre["permission"] == nil {
		t.Fatalf("pre-decision missing: %v", pre)
	}
	// A firing is recorded post-tool; a re-tell then labels it a true violation.
	postOutcome(t, h, map[string]any{"session": "sess-rules-0002", "tool": "Bash", "target": "git push"})
	srv.rulesRetold(target)
	got, _ := st.Get(row.ID)
	if got.LiveTrue != 1 {
		t.Fatalf("retold did not label the firing: %+v", got)
	}
	// Admin surface lists it.
	rec := do(t, h, "GET", "/api/memory/rules?status=enforce", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// A person turns it off.
	if rec := do(t, h, "POST", "/api/memory/rules/disable", map[string]any{"id": row.ID}); rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	out = ctxJSON(t, h, "Bash git push origin main", "stage", "action", "min_rel", "0.9", "session", "sess-rules-0003")
	if out["permission"] != nil {
		t.Fatalf("disabled check still asks: %v", out)
	}
}
