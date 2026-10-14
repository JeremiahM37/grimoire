package api

import (
	"fmt"
	"strings"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

// denyOnceFlag names the experiment behind which a compiled rule may deny an
// action once per session. It is off unless GRIMOIRE_EXP names it, and the
// hook must also set GRIMOIRE_HOOK_ALLOW_DENY=1 before a deny is honoured.
const denyOnceFlag = "denyonce"

// denyLedgerMax bounds how many (session, check) pairs are remembered. The
// oldest pair is forgotten first, so a very long-running server can only
// re-arm a deny for sessions it has not seen in a long while.
const denyLedgerMax = 10000

// denyLedger remembers which compiled checks have already denied an action in
// which session. The zero value is ready to use.
type denyLedger struct {
	mu    sync.Mutex
	seen  map[string]struct{}
	order []string
}

// claim records that check id denied in session and reports whether this is
// the first time. A later firing of the same check in the same session is not
// a claim, so it falls back to ask.
func (l *denyLedger) claim(session, check string) bool {
	key := session + "\x00" + check
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.seen == nil {
		l.seen = map[string]struct{}{}
	}
	if _, ok := l.seen[key]; ok {
		return false
	}
	if len(l.order) >= denyLedgerMax {
		delete(l.seen, l.order[0])
		l.order = l.order[1:]
	}
	l.seen[key] = struct{}{}
	l.order = append(l.order, key)
	return true
}

// denyOnceOn reports whether deny-once may apply to this action: the
// experiment is on and the caller identified its session. Without a session
// there is nothing to scope the once-only rule to, so it never denies.
func denyOnceOn(a actionContext) bool {
	return a.session != "" && exp.On(denyOnceFlag)
}

// denyOnceDecision returns a deny permission if any enforced check that fires
// on this action is a person's rule, or was set to enforce by a person, and has
// not yet denied in this session.
// Each such check is recorded as having denied, so the next firing asks. It
// returns nil when no check qualifies, and the caller keeps the ask.
func (s *Server) denyOnceDecision(session string, hits []rulecheck.Hit, human map[string]bool, minPrecision float64) map[string]any {
	claimed := false
	var reasons []string
	seen := map[string]bool{}
	for _, h := range hits {
		if !h.Enforce {
			continue
		}
		if !seen[h.Row.Target] {
			seen[h.Row.Target] = true
			reasons = append(reasons, "Grimoire rule: "+clipText(oneLine(h.Row.RuleText), 240)+
				" (why: "+clipText(oneLine(h.Row.Reason), 160)+")")
		}
		// A person's authority over this check: either the rule's memory is
		// theirs, or they set this exact check to enforce themselves
		// (`grimoire rules enable --enforce`). A check that reached enforce on
		// measured precision alone, from a rule an agent wrote, only asks.
		byPerson := human[h.Row.Target] || h.Row.User == rulecheck.UserEnforce
		if byPerson && s.denyOnce.claim(session, h.Row.ID) {
			claimed = true
		}
	}
	if !claimed {
		return nil
	}
	return map[string]any{"decision": "deny", "once": true, "reason": strings.Join(reasons, " | ") +
		fmt.Sprintf(" (a compiled check measured at precision >= %.2f flags this action, and the rule is "+
			"a person's; denied once for this session, so tell the user which rule applies and retry only if they allow it)",
			minPrecision)}
}

// oneLine collapses whitespace, so a reason is a single line.
func oneLine(t string) string { return strings.Join(strings.Fields(t), " ") }

// humanRuleTargets returns the targets among items whose memory carries human
// authority. A note target has no authority of its own, so it never qualifies.
func humanRuleTargets(items []contextItem) map[string]bool {
	human := map[string]bool{}
	for _, it := range items {
		if it.Authority == memory.AuthorityHuman.String() {
			human[itemTarget(it)] = true
		}
	}
	return human
}
