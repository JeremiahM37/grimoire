package build

import "testing"

func TestStringPrefersTheStampedVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = "v2.0.0"
	if got := String(); got != "v2.0.0" {
		t.Fatalf("got %q, want the stamped version", got)
	}
	if got := UserAgent(); got != "grimoire/2.0.0" {
		t.Fatalf("user agent %q", got)
	}
}

// An unstamped build must not be able to pass for a release.
func TestUnstampedBuildIsNotAVersion(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })

	Version = ""
	got := String()
	if got == "" {
		t.Fatal("empty version")
	}
	if got[0] == 'v' {
		t.Fatalf("unstamped build reports %q, which reads as a release", got)
	}
}
