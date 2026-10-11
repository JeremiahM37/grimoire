package modelcheck

import (
	"strings"
	"testing"
)

const (
	me     = 0 // human
	claude = 1 // agent
	jira   = 2 // pulled
)

func TestSameValueIsNoop(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, false)
	before := m.Facts[0].State
	d := m.Remember(0, 0, me, false)
	if d.Op != "NOOP" || d.Target != 0 || m.Facts[0].State != before || len(m.Facts) != 1 {
		t.Fatalf("same value should be a NOOP that changes nothing: %+v", d)
	}
}

func TestEqualRungSupersedes(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, false)
	d := m.Remember(0, 1, claude, false)
	if d.Op != "UPDATE" || m.Facts[0].State != Superseded || m.Facts[0].By != 1 {
		t.Fatalf("an agent should correct its own fact: %+v, %+v", d, m.Facts[0])
	}
}

func TestHumanIsNotSupersededByAgent(t *testing.T) {
	var m Model
	m.Remember(0, 0, me, false)
	d := m.Remember(0, 1, claude, false)
	if d.Op != "ADD" || d.Ch != 0 || m.Facts[0].State != Live {
		t.Fatalf("a person's fact must stand and be challenged: %+v, %+v", d, m.Facts[0])
	}
	if got := m.Facts[1].Ch; got != 0 {
		t.Fatalf("the challenger must point at the person's fact, got %d", got)
	}
}

func TestPulledCannotRetractAgentOrHumanFact(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, false)
	if m.Forget(jira, 0) || m.Facts[0].State != Live {
		t.Fatal("pulled text must not retract an agent's fact")
	}
	m.Remember(1, 0, me, false)
	if m.HardForget(claude, 1) || m.Facts[1].State != Live {
		t.Fatal("an agent must not hard-forget a person's fact")
	}
}

func TestPersonRetractionIsAttributedToHuman(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, false)
	if !m.Forget(me, 0) || m.Facts[0].Who != "human" {
		t.Fatalf("a person's forget must be attributed to human: %+v", m.Facts[0])
	}
}

func TestOnlyAPersonResolvesADispute(t *testing.T) {
	var m Model
	m.Remember(0, 0, me, false)
	m.Remember(0, 1, claude, false)
	if m.Resolve(claude, 0, "keep", -1) {
		t.Fatal("an agent must not resolve a dispute")
	}
	if !m.Resolve(me, 0, "keep", -1) {
		t.Fatal("a person must be able to keep their fact")
	}
	if m.Facts[1].State != Retracted || m.Facts[1].Who != "human" || !m.Facts[0].Human {
		t.Fatalf("keep retracts the challenger and confirms the person: %+v %+v", m.Facts[0], m.Facts[1])
	}
}

func TestAcceptMakesTheChallengerHumanAndSupersedes(t *testing.T) {
	var m Model
	m.Remember(0, 0, me, false)
	m.Remember(0, 1, claude, false)
	if !m.Resolve(me, 0, "accept", 1) {
		t.Fatal("accepting the challenger should succeed")
	}
	if m.Facts[0].State != Superseded || m.Facts[0].By != 1 || !m.Facts[1].Human || m.Facts[1].Ch != -1 {
		t.Fatalf("accept supersedes the original and makes the challenger human: %+v %+v", m.Facts[0], m.Facts[1])
	}
}

func TestPrivacyIsInheritedByAReplacement(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, true)
	m.Remember(0, 1, claude, false)
	if !m.Facts[1].Private {
		t.Fatal("a restatement must not publish a private fact")
	}
}

func TestBelievedAtExcludesForgottenAtEveryInstant(t *testing.T) {
	var m Model
	m.Remember(0, 0, claude, false)
	m.Remember(0, 1, claude, false)
	m.HardForget(me, 0)
	if got := m.BelievedAt(0, 1); len(got) != 0 {
		t.Fatalf("a forgotten fact is not believed at any instant, got %d", len(got))
	}
	if got := m.BelievedAt(0, 2); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("at step 2 only the replacement is believed, got %v", got)
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	a, b := Generate(7, 40), Generate(7, 40)
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("op %d differs between runs of one seed", i)
		}
	}
	if c := Generate(8, 40); c[0] == a[0] && c[1] == a[1] && c[2] == a[2] {
		t.Fatal("different seeds should give different sequences")
	}
}

func TestShrinkReducesToTheFailingOp(t *testing.T) {
	ops := Generate(3, 30)
	ops = append(ops, Op{Kind: KProbe, Slot: 1, Sample: 5})
	fails := func(c []Op) bool {
		for _, o := range c {
			if o.Kind == KProbe && o.Slot == 1 {
				return true
			}
		}
		return false
	}
	got := Shrink(ops, fails)
	if len(got) != 1 || got[0].Kind != KProbe {
		t.Fatalf("shrink should keep only the failing op, got %v", got)
	}
}

func TestRegressionTestIsPasteable(t *testing.T) {
	src := RegressionTest("X", []Op{{Kind: KRemember, Slot: 1, Value: 2, Writer: 1}})
	for _, want := range []string{"func TestOracleRegressionX(t *testing.T) {", "runOracle(t, []modelcheck.Op{", "{Kind: modelcheck.KRemember, Slot: 1, Value: 2, Writer: 1},"} {
		if !strings.Contains(src, want) {
			t.Fatalf("regression text missing %q:\n%s", want, src)
		}
	}
}
