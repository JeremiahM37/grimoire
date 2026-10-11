package memory

import (
	"strings"
	"testing"
)

// The verification trailer is new, so it must round-trip exactly and must not
// appear on any bullet that never had it.

// agentEntry is a fact an agent wrote, with an id that matches its content so
// the bullet is not read as a person's hand edit.
func agentEntry(text string) Entry {
	const stamp, agent = "2026-10-09 10:00", "claude"
	return Entry{ID: DeriveID(stamp, agent, text), Text: text, Agent: agent, Stamp: stamp}
}

func TestVerificationRoundTripsByteIdentically(t *testing.T) {
	for _, state := range []string{VerifiedEvidence, UnverifiedEvidence} {
		e := agentEntry("the backup runs at 03:00")
		e.Evidence = "memory/cron.md,https://example.org/x"
		e.Verification = state
		line := e.Format()
		if !strings.Contains(line, " evv="+state) {
			t.Fatalf("evv=%s not written: %s", state, line)
		}
		back := mustParse(t, line)
		if back.Verification != state {
			t.Fatalf("evv=%s parsed as %q", state, back.Verification)
		}
		if got := back.Format(); got != line {
			t.Fatalf("round trip changed the bullet:\n got %s\nwant %s", got, line)
		}
	}
}

func TestVerificationOmittedWhenUnset(t *testing.T) {
	e := agentEntry("the cache is redis")
	e.Evidence = "memory/x.md"
	if line := e.Format(); strings.Contains(line, "evv=") {
		t.Fatalf("an entry with no verification wrote evv=: %s", line)
	}
	// An existing bullet with no evv must still format back to itself.
	old := mustParse(t, "- **2026-10-01 09:00 · claude** — the cache is redis"+
		"<!--m id=9906e851fc60 ev=memory/x.md -->")
	if old.Verification != "" {
		t.Fatalf("a bullet without evv parsed as %q", old.Verification)
	}
}

// A hand-edited or unknown value is dropped, not kept: it must not claim a
// verification that nothing performed.
func TestUnknownVerificationValueIsIgnored(t *testing.T) {
	e := mustParse(t, "- **2026-10-09 10:00 · claude** — x<!--m id=9906e851fc60 ev=a evv=trusted -->")
	if e.Verification != "" {
		t.Fatalf("evv=trusted was kept as %q", e.Verification)
	}
}

// Evidence that did not resolve caps the basis at inferred, whatever the
// category says. Verified evidence keeps the basis it always had.
func TestUnverifiedEvidenceCapsBasisAtInferred(t *testing.T) {
	base := agentEntry("the backup runs at 03:00")
	base.Evidence = "memory/forged.md"

	forged := base
	forged.Verification = UnverifiedEvidence
	if b := forged.Basis(); b != BasisInferred {
		t.Fatalf("unverified evidence read as %s, want inferred", b)
	}
	forged.Category = "observation"
	if b := forged.Basis(); b != BasisInferred {
		t.Fatalf("unverified evidence under an observation category read as %s, want inferred", b)
	}

	real := base
	real.Verification = VerifiedEvidence
	if b := real.Basis(); b != BasisObserved {
		t.Fatalf("verified evidence read as %s, want observed", b)
	}
}

// A person's own fact is stated whatever its evidence says: the cap is a rule
// about agent writes and must not reach human ones.
func TestHumanWriteIsUnaffectedByVerification(t *testing.T) {
	e := agentEntry("the backup runs at 03:00")
	e.Human = true
	e.Evidence = "memory/forged.md"
	e.Verification = UnverifiedEvidence
	if b := e.Basis(); b != BasisStated {
		t.Fatalf("human write read as %s, want stated", b)
	}
}
