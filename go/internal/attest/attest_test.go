package attest

import (
	"strings"
	"testing"
)

// fixture is a small store: two notes, one stored document hash, one pulled URL
// and one entry id. Anything not in it does not resolve.
func fixture() Checker {
	notes := map[string]string{
		"memory/cron.md": "# Cron\n\nThe backup runs at 03:00 every night.\n\n## Deploy steps\n\nRun make.\n",
	}
	const docHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	return Checker{
		Note: func(rel string) (string, bool) {
			b, ok := notes[rel]
			return b, ok
		},
		Hash:    func(h string) bool { return h == docHash },
		Fetched: func(u string) bool { return u == "https://example.org/runbook" },
		Entry:   func(id string) bool { return id == "3f9c2a1b0d4e" },
	}
}

func one(t *testing.T, c Checker, ref string) Result {
	t.Helper()
	rs := c.Verify([]string{ref})
	if len(rs) != 1 {
		t.Fatalf("Verify(%q) returned %d results, want 1", ref, len(rs))
	}
	return rs[0]
}

func TestRealEvidenceResolves(t *testing.T) {
	c := fixture()
	cases := map[string]string{
		"memory/cron.md":                      KindNote,
		"memory/cron.md#backup runs at 03:00": KindNote,
		"memory/cron.md#deploy-steps":         KindNote,
		"sha256:9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08": KindHash,
		"https://example.org/runbook": KindURL,
		"3f9c2a1b0d4e":                KindEntry,
	}
	for ref, kind := range cases {
		r := one(t, c, ref)
		if !r.OK || r.Reason != ReasonOK || r.Kind != kind {
			t.Errorf("%q = %+v, want ok %s", ref, r, kind)
		}
	}
	if s := Status(c.Verify([]string{"memory/cron.md", "https://example.org/runbook"})); s != "verified" {
		t.Fatalf("all-real evidence status = %q, want verified", s)
	}
}

// Forged evidence: each of these names something the store does not hold.
func TestForgedEvidenceDoesNotResolve(t *testing.T) {
	c := fixture()
	cases := map[string]string{
		"memory/never-written.md":                                                 ReasonNoNote,
		"memory/cron.md#the backup runs at 04:00":                                 ReasonQuoteMissing,
		"memory/cron.md#no-such-heading":                                          ReasonQuoteMissing,
		"sha256:0000000000000000000000000000000000000000000000000000000000000000": ReasonNoHash,
		"sha256:not-hex":                                                          ReasonUnrecognised,
		"https://example.com/never-fetched":                                       ReasonNotFetched,
		"000000000000":                                                            ReasonNoEntry,
		"#only-a-fragment":                                                        ReasonUnrecognised,
	}
	for ref, reason := range cases {
		r := one(t, c, ref)
		if r.OK || r.Reason != reason {
			t.Errorf("%q = %+v, want unverified with reason %s", ref, r, reason)
		}
	}
}

// A mixed list is unverified as a whole: one forged reference taints the set,
// because the basis is a claim about the whole citation.
func TestOneForgedReferenceMakesTheSetUnverified(t *testing.T) {
	c := fixture()
	if s := Status(c.Verify([]string{"memory/cron.md", "memory/forged.md"})); s != "unverified" {
		t.Fatalf("mixed evidence status = %q, want unverified", s)
	}
}

// Without a resolver for a kind, the reference is not checked, which is
// unverified: an unconfigured store must not look as if it had checked.
func TestNilResolverIsNotChecked(t *testing.T) {
	var c Checker
	for _, ref := range []string{"memory/a.md", "sha256:" + strings.Repeat("a", 64), "https://x.test/", "0123456789ab"} {
		r := one(t, c, ref)
		if r.OK || r.Reason != ReasonNotChecked {
			t.Errorf("%q with no resolver = %+v, want not_checked", ref, r)
		}
	}
}

// An unreadable note is reported as absent, not forbidden, so the check does
// not confirm that a note by that name exists.
func TestUnreadableNoteReadsAsAbsent(t *testing.T) {
	c := fixture()
	c.Note = func(string) (string, bool) { return "", false }
	if r := one(t, c, "memory/cron.md"); r.OK || r.Reason != ReasonNoNote {
		t.Fatalf("unreadable note = %+v, want no_such_note", r)
	}
}

func TestNoEvidenceHasNoStatus(t *testing.T) {
	if got := (Checker{}).Verify(nil); len(got) != 0 {
		t.Fatalf("Verify(nil) = %v, want empty", got)
	}
	if s := Status(nil); s != "" {
		t.Fatalf("Status(nil) = %q, want empty so no trailer is written", s)
	}
	if got := (Checker{}).Verify([]string{"  ", ""}); len(got) != 0 {
		t.Fatalf("blank references should be dropped, got %v", got)
	}
}

func TestSlugMatchesHeadingAnchors(t *testing.T) {
	cases := map[string]string{"Deploy steps": "deploy-steps", "Step 2: Restart!": "step-2-restart", "Café menu": "café-menu"}
	for in, want := range cases {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}
