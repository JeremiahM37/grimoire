package memory

import (
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func TestValidityRoundTripsThroughTheBullet(t *testing.T) {
	want := Entry{ID: "v1", Stamp: "2026-08-14 09:00", Agent: "claude",
		Text: "the office is on floor three", ValidFrom: "2026-03-01T00:00:00Z",
		ValidTo: "2026-09-01"}
	got, ok := ParseLine(want.Format())
	if !ok {
		t.Fatal("did not parse")
	}
	if got != want {
		t.Errorf("round trip mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestLegacyBulletsWithoutValidityAreUnchanged(t *testing.T) {
	// A bullet written before validity existed must parse to an entry with no
	// validity, and re-format to the same bytes.
	id := DeriveID("2026-08-14 09:00", "claude", "the deploy host is ember")
	legacy := "- ~~**2026-08-14 09:00 · claude** — the deploy host is ember~~ " +
		"<!--m id=" + id + " sup=ffeeddccbbaa supat=2026-08-15\\s09:00-->"
	e, ok := ParseLine(legacy)
	if !ok {
		t.Fatal("legacy bullet did not parse")
	}
	if e.ValidFrom != "" || e.ValidTo != "" {
		t.Errorf("legacy bullet gained validity: %q / %q", e.ValidFrom, e.ValidTo)
	}
	if got := e.Format(); got != legacy {
		t.Errorf("legacy bullet did not round-trip byte-identically\n got: %s\nwant: %s", got, legacy)
	}
}

func TestHandEditedValidityIsReadAsWritten(t *testing.T) {
	// A person correcting a date in the trailer must win: the parser reads
	// exactly what is in the file, and a malformed bound reads as unbounded
	// rather than hiding the fact.
	line := "- **2026-08-14 09:00 · claude** — on call is priya " +
		"<!--m id=abc123def456 valid_to=2026-10-01T00:00:00Z-->"
	e, ok := ParseLine(line)
	if !ok {
		t.Fatal("did not parse")
	}
	if e.ValidTo != "2026-10-01T00:00:00Z" {
		t.Errorf("hand-set valid_to read as %q", e.ValidTo)
	}
	bad, _ := ParseLine("- **2026-08-14 09:00 · claude** — x <!--m id=abc123def456 valid_from=last-tuesday-->")
	if !bad.ValidAt(mustTime(t, "2020-01-01T00:00:00Z")) {
		t.Error("an unparseable valid_from hid the fact")
	}
}

func TestValidAtIsHalfOpen(t *testing.T) {
	e := Entry{ValidFrom: "2026-03-01", ValidTo: "2026-09-01T00:00:00Z"}
	cases := map[string]bool{
		"2026-02-28T23:59:59Z": false, // before valid_from
		"2026-03-01T00:00:00Z": true,  // valid_from is inclusive
		"2026-08-31T23:59:59Z": true,
		"2026-09-01T00:00:00Z": false, // valid_to is exclusive
	}
	for at, want := range cases {
		if got := e.ValidAt(mustTime(t, at)); got != want {
			t.Errorf("ValidAt(%s) = %v, want %v", at, got, want)
		}
	}
}

func TestFactsWithoutValidityAreAlwaysValid(t *testing.T) {
	e := Entry{ID: "a"}
	for _, at := range []string{"1970-01-01T00:00:00Z", "2099-01-01T00:00:00Z"} {
		if !e.ValidAt(mustTime(t, at)) {
			t.Errorf("an unbounded fact was not valid at %s", at)
		}
	}
	if !e.ValidDuring(mustTime(t, "2001-01-01T00:00:00Z"), time.Time{}) {
		t.Error("an unbounded fact missed a range")
	}
}

func TestValidDuringIsOverlapNotContainment(t *testing.T) {
	e := Entry{ValidFrom: "2026-03-01", ValidTo: "2026-03-10"}
	year := func(from, to string) (time.Time, time.Time) {
		return mustTime(t, from), mustTime(t, to)
	}
	if s, u := year("2026-01-01T00:00:00Z", "2026-12-31T00:00:00Z"); !e.ValidDuring(s, u) {
		t.Error("a short fact inside a long range did not overlap it")
	}
	if s, u := year("2026-03-10T00:00:00Z", "2026-04-01T00:00:00Z"); e.ValidDuring(s, u) {
		t.Error("a range starting at valid_to overlapped the fact (valid_to is exclusive)")
	}
	if s, u := year("2026-02-01T00:00:00Z", "2026-02-28T00:00:00Z"); e.ValidDuring(s, u) {
		t.Error("a range entirely before the fact overlapped it")
	}
	if !e.ValidDuring(time.Time{}, mustTime(t, "2026-03-01T00:00:00Z")) {
		t.Error("an open-start range ending at valid_from did not overlap (valid_from is inclusive)")
	}
}

func TestDatesAreUTCMidnight(t *testing.T) {
	// The answer must not depend on the machine's timezone, so a date bound is
	// midnight UTC.
	e := Entry{ValidFrom: "2026-03-01"}
	if !e.ValidAt(mustTime(t, "2026-03-01T00:00:00Z")) {
		t.Error("a date bound was not midnight UTC")
	}
}

func TestNormValidity(t *testing.T) {
	cases := map[string]string{
		"":                          "",
		"  ":                        "",
		"2026-05-01":                "2026-05-01T00:00:00Z",
		"2026-05-01T10:00:00+02:00": "2026-05-01T08:00:00Z",
	}
	for in, want := range cases {
		got, err := NormValidity(in)
		if err != nil {
			t.Errorf("NormValidity(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("NormValidity(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := NormValidity("last tuesday"); err == nil || !strings.Contains(err.Error(), "RFC3339") {
		t.Errorf("a non-time was accepted: %v", err)
	}
}

func TestCloseValidityEndsTheOldFactWhereTheNewOneBegins(t *testing.T) {
	old := Entry{ID: "old", ValidFrom: "2026-01-01T00:00:00Z"}
	if !CloseValidity(&old, "2026-05-01T00:00:00Z") {
		t.Fatal("an open-ended fact was not closed")
	}
	if old.ValidTo != "2026-05-01T00:00:00Z" {
		t.Errorf("valid_to = %q", old.ValidTo)
	}
}

func TestCloseValidityNeverOverwritesAHumanBound(t *testing.T) {
	old := Entry{ID: "old", ValidTo: "2026-02-01T00:00:00Z"}
	if CloseValidity(&old, "2026-05-01T00:00:00Z") || old.ValidTo != "2026-02-01T00:00:00Z" {
		t.Errorf("an existing valid_to was overwritten: %q", old.ValidTo)
	}
}

func TestCloseValidityRefusesAnInvertedInterval(t *testing.T) {
	// A replacement that starts before the old fact did is a history correction,
	// and closing on it would leave valid_to before valid_from.
	old := Entry{ID: "old", ValidFrom: "2026-05-01T00:00:00Z"}
	if CloseValidity(&old, "2026-01-01T00:00:00Z") || old.ValidTo != "" {
		t.Errorf("closed to an inverted interval: %q", old.ValidTo)
	}
}

func TestAsOfDoesNotApplyAClosureThatWasNotYetKnown(t *testing.T) {
	// The old fact was superseded on Aug 15, which closed its valid_to at March.
	// On Aug 13 that closure had not happened, so March must still read valid.
	e := Entry{ID: "old", Stamp: "2026-08-10 09:00", Text: "floor two",
		ValidFrom: "2026-01-01", ValidTo: "2026-03-01", SupersededBy: "new",
		SupersededAt: "2026-08-15 09:00"}
	march := mustTime(t, "2026-03-15T00:00:00Z")
	aug13 := mustTime(t, "2026-08-13T00:00:00Z")
	aug20 := mustTime(t, "2026-08-20T00:00:00Z")
	if !e.ValidAtAsOf(march, aug13) {
		t.Error("a bound learned on Aug 15 was applied to a view as of Aug 13")
	}
	if e.ValidAtAsOf(march, aug20) {
		t.Error("the closure was ignored once it was known")
	}
	if e.ValidAt(march) {
		t.Error("the current view ignored the closure")
	}
}
