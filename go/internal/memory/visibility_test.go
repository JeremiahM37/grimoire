package memory

import (
	"strings"
	"testing"
)

// The visibility tag is the same kind of change as imp= and ev=: absent means
// normal, a bullet written before it existed comes back byte for byte, and a
// hidden fact is marked in its trailer where a person can see it.

func TestVisibilityRoundTripsAndNormalIsNotWritten(t *testing.T) {
	stamp, agent, text := "2026-08-14 09:00", "claude", "prefers tabs"
	id := DeriveID(stamp, agent, text)
	old := "- **" + stamp + " · " + agent + "** — " + text + " <!--m id=" + id + " cat=preference-->"
	e, ok := ParseLine(old)
	if !ok {
		t.Fatal("old bullet did not parse")
	}
	if e.Visibility != "" || e.Hidden() {
		t.Errorf("a bullet without vis= parsed as %q", e.Visibility)
	}
	if got := e.Format(); got != old {
		t.Errorf("old bullet changed on round trip:\n got %q\nwant %q", got, old)
	}

	for _, vis := range []string{VisPrivate, VisSensitive} {
		h := e
		h.Visibility = vis
		line := h.Format()
		if !strings.Contains(line, "vis="+vis) {
			t.Fatalf("%s bullet lacks vis=%s: %q", vis, vis, line)
		}
		back, _ := ParseLine(line)
		if back.Visibility != vis || back.Format() != line || !back.Hidden() {
			t.Errorf("%s bullet did not round trip: %q -> %q", vis, line, back.Visibility)
		}
	}

	// normal is accepted on the way in and never written out.
	n := e
	n.Visibility = VisNormal
	if strings.Contains(n.Format(), "vis=") {
		t.Errorf("normal visibility was written: %q", n.Format())
	}
}

func TestUnknownVisibilityFailsClosed(t *testing.T) {
	// A hand-edited typo must hide a fact, not publish it.
	line := "- **2026-08-14 09:00 · claude** — prefers tabs <!--m id=x vis=secret-->"
	e, ok := ParseLine(line)
	if !ok || e.Visibility != VisPrivate || !e.Hidden() {
		t.Fatalf("vis=secret parsed as %q (hidden=%v), want private", e.Visibility, e.Hidden())
	}
	// Only sensitive redacts; private does not.
	p := Entry{Visibility: VisPrivate}
	if p.Redacted() || !p.Hidden() {
		t.Error("private must hide without redacting")
	}
	s := Entry{Visibility: VisSensitive}
	if !s.Redacted() || !s.Hidden() {
		t.Error("sensitive must hide and redact")
	}
}

func TestNormVisibility(t *testing.T) {
	for in, want := range map[string]string{"": "", "normal": "", " Private ": VisPrivate, "sensitive": VisSensitive} {
		got, err := NormVisibility(in)
		if err != nil || got != want {
			t.Errorf("NormVisibility(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := NormVisibility("public"); err == nil {
		t.Error("an unknown visibility was accepted")
	}
}

func TestDropHiddenKeepsEveryOtherLineExactly(t *testing.T) {
	stamp, agent := "2026-08-14 09:00", "claude"
	open := Entry{ID: DeriveID(stamp, agent, "open fact"), Stamp: stamp, Agent: agent, Text: "open fact"}
	secret := Entry{ID: DeriveID(stamp, agent, "secret fact"), Stamp: stamp, Agent: agent,
		Text: "secret fact", Visibility: VisPrivate}
	body := "# Memory: ops\n\n" + open.Format() + "\n" + secret.Format() + "\n\nfree text\n"

	got := DropHidden(body)
	want := "# Memory: ops\n\n" + open.Format() + "\n\nfree text\n"
	if got != want {
		t.Errorf("DropHidden:\n got %q\nwant %q", got, want)
	}

	// A body with no hidden fact comes back unchanged, byte for byte.
	plain := "# Memory\n\n" + open.Format() + "\n"
	if DropHidden(plain) != plain {
		t.Error("DropHidden changed a body with no hidden fact")
	}
}
