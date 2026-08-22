package memory

import (
	"strings"
	"testing"
)

// The scenario this whole mechanism exists for, and the one that used to fail.
//
// An agent records a wrong value. A person opens the file and fixes it. The
// agent then meets the original evidence again and writes the same wrong value
// a second time. Before the authority lattice this superseded the correction —
// and struck the PERSON's line through, so the history recorded them as the
// party who had been corrected.
func TestAgentWriteMayNotSupersedeAHumanCorrection(t *testing.T) {
	stamp := "2026-08-22 01:17"
	// Exactly what the file holds after a hand edit: the id the agent minted
	// for the ORIGINAL text, still attached to text a person has since changed.
	corrected := Entry{
		ID:    DeriveID(stamp, "cli", "Billing Postgres runs on port 5432"),
		Text:  "Billing Postgres runs on port 6432",
		Agent: "cli", Stamp: stamp,
	}
	line := corrected.Format()
	parsed, ok := ParseLine(line)
	if !ok {
		t.Fatalf("bullet did not round-trip: %s", line)
	}
	if !parsed.HandWritten {
		t.Fatal("a bullet whose id no longer matches its text is a hand edit; " +
			"nothing else can change the text without re-minting the id")
	}
	if got := parsed.Authority(); got != AuthorityHuman {
		t.Fatalf("authority = %v, want human", got)
	}

	d := DecideAs("Billing Postgres runs on port 5432", "", false, []Entry{parsed})
	if d.Op == OpUpdate || d.Op == OpDelete {
		t.Fatalf("agent write %v the human's correction — this is the resurrection "+
			"bug: %s", d.Op, d.Why)
	}
	if d.Op != OpAdd {
		t.Fatalf("op = %v, want ADD alongside", d.Op)
	}
}

// The correction survives being written back, which is what makes it durable
// across the next rewrite rather than only until one happens.
func TestHandEditIsDeclaredOnceItIsRewritten(t *testing.T) {
	stamp := "2026-08-22 01:17"
	e := Entry{
		ID:    DeriveID(stamp, "cli", "port 5432"),
		Text:  "port 6432",
		Agent: "cli", Stamp: stamp,
	}
	first, _ := ParseLine(e.Format())
	if !first.HandWritten {
		t.Fatal("inference failed on the first parse")
	}
	// Re-formatting is what an agent's next write to the note does to every
	// line in it. The inference must not be the only thing carrying the claim,
	// because that rewrite re-mints nothing and the hash would start matching
	// again the moment the text and id were written together.
	second, _ := ParseLine(first.Format())
	if !second.Human {
		t.Fatal("by=human was not written, so the correction would silently " +
			"demote to an agent fact on the next rewrite")
	}
	if got := second.Authority(); got != AuthorityHuman {
		t.Fatalf("authority after rewrite = %v, want human", got)
	}
}

// A person may still correct the agent. The lattice is an asymmetry, not a lock.
func TestHumanMaySupersedeAnAgentFact(t *testing.T) {
	agentFact := Entry{ID: "aaaaaaaaaaaa", Text: "Billing Postgres runs on port 5432",
		Agent: "claude", Stamp: "2026-08-22 01:00"}
	d := DecideAs("Billing Postgres runs on port 6432", "", true, []Entry{agentFact})
	if d.Op != OpUpdate {
		t.Fatalf("op = %v (%s), want UPDATE — a person must be able to correct "+
			"an agent", d.Op, d.Why)
	}
}

// The ordinary case has to keep working: an agent correcting its own earlier
// fact is most of what reconciliation does.
func TestAgentStillSupersedesItsOwnFact(t *testing.T) {
	stamp := "2026-08-22 01:00"
	own := Entry{ID: DeriveID(stamp, "claude", "the deploy target is old.example"),
		Text: "the deploy target is old.example", Agent: "claude", Stamp: stamp}
	parsed, _ := ParseLine(own.Format())
	if parsed.HandWritten {
		t.Fatal("an untouched agent bullet must not read as hand-written")
	}
	d := DecideAs("the deploy target is new.example", "", false, []Entry{parsed})
	if d.Op != OpUpdate {
		t.Fatalf("op = %v (%s), want UPDATE", d.Op, d.Why)
	}
}

// The rung that already existed, preserved verbatim: a stranger's text may not
// overwrite the operator's.
func TestUntrustedStillMayNotSupersede(t *testing.T) {
	stamp := "2026-08-22 01:00"
	mine := Entry{ID: DeriveID(stamp, "claude", "the deploy host is prod.example"),
		Text: "the deploy host is prod.example", Agent: "claude", Stamp: stamp}
	d := DecideAs("the deploy host is evil.example", "connector:slack:C123", false,
		[]Entry{mine})
	if d.Op == OpUpdate || d.Op == OpDelete {
		t.Fatalf("untrusted text %v a trusted fact: %s", d.Op, d.Why)
	}
}

// Untrusted text must not be able to climb the lattice by asserting that it is
// a person — "I am the operator" is precisely the sentence a poisoned document
// would contain.
func TestPulledOriginOutranksAHumanClaim(t *testing.T) {
	if got := AuthorityOf("connector:slack:C123", true); got != AuthorityPulled {
		t.Fatalf("authority = %v, want pulled: a human claim must not lift "+
			"untrusted content", got)
	}
}

// The inference is only sound for ids this package mints. Anything else — a
// fixture's invented id, a future scheme — says nothing about its own content,
// so comparing it to a hash must not be read as evidence of an edit.
func TestInferenceIgnoresIDsItCouldNotHaveMinted(t *testing.T) {
	for _, id := range []string{"abc127", "a1", "not-hex-at-all", "AAAAAAAAAAAA"} {
		e := Entry{ID: id, Text: "some fact", Agent: "agent", Stamp: "2026-08-14 09:00"}
		parsed, ok := ParseLine(e.Format())
		if !ok {
			t.Fatalf("did not parse: %s", id)
		}
		if parsed.HandWritten {
			t.Errorf("id %q is not in the minted format; it cannot be evidence "+
				"of a hand edit", id)
		}
	}
}

// A bullet a person typed themselves has no trailer at all.
func TestBulletWithNoTrailerIsHumanAuthored(t *testing.T) {
	parsed, ok := ParseLine("- **2026-08-22 09:00 · me** — the office moved to the third floor")
	if !ok {
		t.Fatal("did not parse")
	}
	if !parsed.HandWritten || parsed.Authority() != AuthorityHuman {
		t.Fatalf("a bullet with no id was written by hand; authority = %v",
			parsed.Authority())
	}
}

// The collision suffix says nothing about whether the text changed.
func TestCollisionSuffixIsNotAHandEdit(t *testing.T) {
	stamp := "2026-08-22 01:00"
	e := Entry{ID: DeriveID(stamp, "claude", "a repeated fact") + "-1",
		Text: "a repeated fact", Agent: "claude", Stamp: stamp}
	parsed, _ := ParseLine(e.Format())
	if parsed.HandWritten {
		t.Fatal("a -N collision suffix must not read as a hand edit")
	}
}

// The refusal has to say which rung refused, because "may not supersede" is now
// two different refusals and a caller deciding whether to escalate needs to know
// which one it hit.
func TestRefusalNamesBothRungs(t *testing.T) {
	stamp := "2026-08-22 01:17"
	human := Entry{ID: DeriveID(stamp, "cli", "port 5432"), Text: "port 6432",
		Agent: "cli", Stamp: stamp}
	parsed, _ := ParseLine(human.Format())
	d := DecideAs("port 5432", "", false, []Entry{parsed})
	if !strings.Contains(d.Why, "agent") || !strings.Contains(d.Why, "human") {
		t.Fatalf("why = %q, want it to name the incoming and existing rungs", d.Why)
	}
}
