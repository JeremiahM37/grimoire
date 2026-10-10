package memory

import "testing"

// An image reference rides in the trailer like every other field, and a bullet
// without one must format exactly as it did before the field existed.
func TestImageTrailerRoundTrips(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	id := DeriveID("2026-08-14 09:00", "agent", "network diagram of rack B")
	line := "- **2026-08-14 09:00 · agent** — network diagram of rack B <!--m id=" + id +
		" img=" + sha + " capb=stated-->"
	if e, _ := ParseLine(line); e.HandWritten {
		t.Fatal("fixture id must match its content")
	}
	e, ok := ParseLine(line)
	if !ok {
		t.Fatal("bullet did not parse")
	}
	if e.Image != sha || e.CaptionBasis != CaptionStated || e.Text != "network diagram of rack B" {
		t.Fatalf("parsed %+v", e)
	}
	if got := e.Format(); got != line {
		t.Fatalf("round trip changed the bullet:\n got %s\nwant %s", got, line)
	}
}

func TestBulletsWithoutImageAreByteIdentical(t *testing.T) {
	id := DeriveID("2026-08-14 09:00", "agent", "the deploy host is on tailscale")
	line := "- **2026-08-14 09:00 · agent** — the deploy host is on tailscale <!--m id=" + id +
		" cat=fact imp=4 pr=0.5-->"
	e, ok := ParseLine(line)
	if !ok {
		t.Fatal("bullet did not parse")
	}
	if e.Image != "" || e.CaptionBasis != "" {
		t.Fatalf("image fields invented for a plain fact: %+v", e)
	}
	if got := e.Format(); got != line {
		t.Fatalf("plain bullet changed on rewrite:\n got %s\nwant %s", got, line)
	}
}
