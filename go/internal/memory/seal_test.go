package memory

import "testing"

// A sealed bullet round-trips byte for byte, and its id is checked against the
// ciphertext, not the placeholder. Without that the id would read as a hand
// edit on every parse and the fact would be attributed to a person.
func TestSealedBulletRoundTripsAndIsNotHandEdited(t *testing.T) {
	t.Parallel()
	e := Entry{Stamp: "2026-10-10 09:00", Agent: "claude", Text: SealedPlaceholder,
		Visibility: VisSealed, Owner: "user:jam", KeyID: "0123abcd", Sealed: "gAAAAAB-token_x="}
	e.ID = DeriveID(e.Stamp, e.Agent, e.Sealed)
	line := e.Format()
	got, ok := ParseLine(line)
	if !ok {
		t.Fatalf("sealed bullet did not parse: %s", line)
	}
	if got.Format() != line {
		t.Fatalf("round trip changed the bullet:\n%s\n%s", line, got.Format())
	}
	if got.HandWritten || got.Human {
		t.Fatalf("a sealed bullet read as hand-edited: %+v", got)
	}
	if got.Sealed != e.Sealed || got.KeyID != e.KeyID || got.Owner != e.Owner || got.Text != SealedPlaceholder {
		t.Fatalf("sealed fields lost in parse: %+v", got)
	}
	if !got.Hidden() || !got.Redacted() {
		t.Fatal("a sealed fact must be hidden and redacted")
	}
}

func TestSealedVisibilityIsNormalisedAndUnknownStillFailsClosed(t *testing.T) {
	t.Parallel()
	if v, err := NormVisibility("Sealed"); err != nil || v != VisSealed {
		t.Fatalf("NormVisibility(Sealed) = %q, %v", v, err)
	}
	if v := parseVisibility("sealedd"); v != VisPrivate {
		t.Fatalf("an unknown vis= value must hide the fact as private, got %q", v)
	}
}

// Ordinary bullets are byte-identical to what they were before sealing existed.
func TestOrdinaryBulletFormatIsUnchangedBySealing(t *testing.T) {
	t.Parallel()
	e := Entry{Stamp: "2026-10-10 09:00", Agent: "claude", Text: "plain fact", ID: "aaaaaaaaaaaa"}
	want := "- **2026-10-10 09:00 · claude** — plain fact <!--m id=aaaaaaaaaaaa-->"
	if got := e.Format(); got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}
