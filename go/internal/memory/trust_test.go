package memory

import (
	"strings"
	"testing"
)

// What a fact's provenance changes about reconciliation.
//
// The engine's job is to let a new fact overwrite an old one. That is exactly
// the capability an injected instruction wants, so the source of a fact has to
// bound what it may overwrite.

func TestAnUntrustedFactCannotSupersedeATrustedOne(t *testing.T) {
	known := []Entry{{ID: "a1", Text: "the deploy host is prod-1.internal"}}
	d := DecideFrom("the deploy host is evil.example",
		"connector:jira:PROJ-4", known)

	if d.Op != OpAdd {
		t.Fatalf("op = %s (%s), want ADD — an untrusted source overwrote a trusted fact",
			d.Op, d.Why)
	}
	if d.Target != "" {
		t.Errorf("decision targets %q; an ADD replaces nothing", d.Target)
	}
	if !strings.Contains(d.Why, "may not supersede") {
		t.Errorf("why = %q — the refusal has to be legible to the caller", d.Why)
	}
}

func TestAnUntrustedFactCannotRetractATrustedOne(t *testing.T) {
	// The DELETE path is the more dangerous half: it removes a belief and
	// offers nothing in its place, so a successful one leaves the agent with
	// no answer rather than a wrong one.
	known := []Entry{{ID: "a1", Text: "the user prefers tabs"}}
	d := DecideFrom("the user no longer prefers tabs", "web:evil.example", known)
	if d.Op == OpDelete {
		t.Fatalf("an untrusted source retracted a trusted fact: %+v", d)
	}
}

func TestATrustedFactStillSupersedesNormally(t *testing.T) {
	// The rule must not cost the engine its actual job.
	known := []Entry{{ID: "a1", Text: "the user prefers spaces"}}
	d := DecideFrom("the user prefers tabs", "", known)
	if d.Op != OpUpdate || d.Target != "a1" {
		t.Fatalf("trusted write got %s/%q, want UPDATE of a1 (%s)", d.Op, d.Target, d.Why)
	}
	// And through the original entry point, which is what every existing
	// caller still uses.
	if d2 := Decide("the user prefers tabs", known); d2.Op != OpUpdate {
		t.Fatalf("Decide (no origin) got %s, want UPDATE", d2.Op)
	}
}

func TestUntrustedMaySupersedeUntrusted(t *testing.T) {
	// A connector re-pulling an edited document must be able to update what it
	// said before. Refusing this would make every re-sync accumulate a
	// contradiction with itself.
	known := []Entry{{ID: "a1", Text: "the release train runs on tuesdays",
		Origin: "connector:slack:C1"}}
	d := DecideFrom("the release train runs on thursdays", "connector:slack:C1", known)
	if d.Op != OpUpdate || d.Target != "a1" {
		t.Fatalf("untrusted-over-untrusted got %s/%q, want UPDATE (%s)", d.Op, d.Target, d.Why)
	}
}

func TestAnUntrustedDuplicateIsStillANoop(t *testing.T) {
	// Blocking supersession must not turn "we already know this" into a
	// duplicate bullet on every sync.
	known := []Entry{{ID: "a1", Text: "the deploy host is prod-1.internal"}}
	d := DecideFrom("the deploy host is prod-1.internal", "connector:jira:P-1", known)
	if d.Op != OpNoop {
		t.Fatalf("op = %s (%s), want NOOP for an exact restatement", d.Op, d.Why)
	}
}

func TestOriginRoundTripsThroughTheBullet(t *testing.T) {
	// Provenance that does not survive a rewrite is provenance that disappears
	// the first time anything edits the note — and the note IS the store.
	e := Entry{ID: "x1", Text: "the staging box is smaller",
		Agent: "ops", Stamp: "2026-08-21 10:00", Origin: "connector:slack:C123"}
	line := e.Format()
	back, ok := ParseLine(line)
	if !ok {
		t.Fatalf("formatted bullet did not parse back:\n%s", line)
	}
	if back.Origin != e.Origin {
		t.Errorf("origin %q -> %q", e.Origin, back.Origin)
	}
	if !back.Untrusted() {
		t.Error("a fact from a connector parsed back as trusted")
	}
}

func TestAFactWithNoOriginIsTrusted(t *testing.T) {
	// Every bullet written before this field existed.
	e, ok := ParseLine("- **2026-01-01 09:00 · agent** — an older fact <!--m id=old1-->")
	if !ok {
		t.Fatal("did not parse")
	}
	if e.Untrusted() {
		t.Error("a bullet with no origin must stay trusted")
	}
}

func TestOriginSurvivesAValueContainingSpaces(t *testing.T) {
	// The trailer is space-separated; an unescaped value ends the field early
	// and silently drops everything after it.
	e := Entry{ID: "x1", Text: "a fact", Stamp: "2026-08-21 10:00", Agent: "ops",
		Origin: "web:example.com/some page"}
	back, ok := ParseLine(e.Format())
	if !ok {
		t.Fatal("did not parse")
	}
	if back.Origin != e.Origin {
		t.Errorf("origin %q -> %q", e.Origin, back.Origin)
	}
}
