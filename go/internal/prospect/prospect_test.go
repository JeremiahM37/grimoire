package prospect

import (
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

func day(s string) time.Time {
	d, err := time.Parse("2006-01-02", s)
	if err != nil {
		panic(err)
	}
	return d.Add(9 * time.Hour)
}

func TestParseCueAcceptsEveryKindAndRoundTrips(t *testing.T) {
	t.Parallel()
	for _, s := range []string{
		"path:deploy/**",
		"path:/etc/lectern.env",
		"cmd:docker compose down",
		"project:grimoire",
		"after:2026-11-01",
		"before:2026-10-20",
		"term:kestrel",
		"path:deploy/*.yml&term:staging&project:grimoire",
	} {
		c, err := ParseCue(s)
		if err != nil {
			t.Fatalf("ParseCue(%q): %v", s, err)
		}
		if c.String() != s {
			t.Fatalf("canonical form of %q is %q", s, c.String())
		}
	}
}

func TestParseCueRefusesWhatItCannotEvaluate(t *testing.T) {
	t.Parallel()
	long := "term:" + strings.Repeat("a", MaxArg+1)
	for _, s := range []string{
		"",
		"deploy",                             // no predicate
		"file:/etc/passwd",                   // not an intention predicate (skillgate's vocabulary)
		"term:",                              // no argument
		"after:2026-13-40",                   // not a date
		"before:tomorrow",                    // not a date
		"term:two words",                     // a term is one word
		"project:a/b",                        // one project
		"path:../secrets/**",                 // escapes the tree
		"path:deploy\\x",                     // backslash
		"cmd:echo <x>",                       // trailer-breaking characters
		"term:x&term:y&term:z&term:v&term:w", // more than MaxTerms
		long,
		strings.Repeat("term:a&", 60),
	} {
		if _, err := ParseCue(s); err == nil {
			t.Fatalf("ParseCue(%q) accepted a cue it cannot evaluate", s)
		}
	}
}

func TestPathGlobMatchesAtComponentBoundaries(t *testing.T) {
	t.Parallel()
	cases := []struct {
		glob, file string
		want       bool
	}{
		{"deploy/*", "deploy/a.yml", true},
		{"deploy/*", "/home/me/repo/deploy/a.yml", true},
		{"deploy/*", "/home/me/repo/notdeploy/a.yml", false},
		{"deploy/*", "/home/me/repo/deploy/sub/a.yml", false},
		{"deploy/**", "/home/me/repo/deploy/sub/a.yml", true},
		{"deploy/**", "deploy", true},
		{"*.env", "/etc/lectern.env", true},
		{"/etc/lectern.env", "/etc/lectern.env", true},
		{"/etc/lectern.env", "/home/x/etc/lectern.env", false},
		{"/etc/**", "/etc/a/b", true},
		{"deploy/*", "", false},
	}
	for _, c := range cases {
		if got := pathMatches(c.glob, c.file); got != c.want {
			t.Errorf("pathMatches(%q, %q) = %v, want %v", c.glob, c.file, got, c.want)
		}
	}
}

func TestTermsHoldOnWholeWordsAndCommandsOnBoundaries(t *testing.T) {
	t.Parallel()
	s := Situation{Now: day("2026-10-11"), Prompt: "Bump the Kestrel build", Command: "docker compose down --volumes"}
	for cue, want := range map[string]bool{
		"term:kestrel":                      true,
		"term:kest":                         false, // a prefix is not the word
		"term:bump&term:build":              true,
		"cmd:docker compose":                true,
		"cmd:docker comp":                   false,
		"cmd:docker compose down --volumes": true,
		"cmd:docker compose down --vol":     false,
	} {
		c, err := ParseCue(cue)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Holds(s); got != want {
			t.Errorf("%s holds = %v, want %v", cue, got, want)
		}
	}
}

func TestProjectAndDatesAreExactAndAnded(t *testing.T) {
	t.Parallel()
	s := Situation{Now: day("2026-10-31"), Project: "Grimoire"}
	for cue, want := range map[string]bool{
		"project:grimoire":                  true,
		"project:other":                     false,
		"after:2026-10-31":                  true,
		"after:2026-11-01":                  false,
		"before:2026-11-01":                 true,
		"before:2026-10-31":                 false,
		"after:2026-10-01&project:grimoire": true,
		"after:2026-10-01&project:other":    false,
	} {
		c, err := ParseCue(cue)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.Holds(s); got != want {
			t.Errorf("%s holds = %v, want %v", cue, got, want)
		}
	}
	if (Cue{}).Holds(s) {
		t.Fatal("an empty cue held")
	}
}

func TestNormLifeAndLiveness(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{"": "", "standing": "", "once": "once", "until:2026-10-15": "until:2026-10-15"} {
		got, err := NormLife(in)
		if err != nil || got != want {
			t.Fatalf("NormLife(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"always", "until:soon", "twice"} {
		if _, err := NormLife(bad); err == nil {
			t.Fatalf("NormLife(%q) accepted", bad)
		}
	}
	// until is inclusive of its UTC day, and nothing is live after retiring.
	if !Live("until:2026-10-15", "", day("2026-10-15")) || Live("until:2026-10-15", "", day("2026-10-16")) {
		t.Fatal("until is not inclusive of its day")
	}
	if Live("once", "2026-10-11T09:00:00Z", day("2026-10-12")) {
		t.Fatal("a retired intention is still live")
	}
	if !Live("", "", day("2030-01-01")) {
		t.Fatal("a standing intention expired")
	}
}

func intention(id, cue, life string) memory.Entry {
	return memory.Entry{ID: id, Text: "remember " + id, Stamp: "2026-10-01 09:00", Cue: cue, Life: life}
}

func TestFiresRefusesEveryNonFiringCase(t *testing.T) {
	t.Parallel()
	s := Situation{Now: day("2026-10-11"), Prompt: "kestrel"}
	ok := intention("ok", "term:kestrel", "")
	if !Fires(ok, s) {
		t.Fatal("a plain matching intention did not fire")
	}
	untrusted := ok
	untrusted.Origin = "connector:slack:C1"
	superseded := ok
	superseded.SupersededBy = "other"
	retired := intention("r", "term:kestrel", "once")
	retired.Retired = "2026-10-10T09:00:00Z"
	private := ok
	private.Visibility = memory.VisPrivate
	notIntention := memory.Entry{ID: "n", Text: "kestrel", Stamp: "2026-10-01 09:00"}
	malformed := intention("bad", "file:/etc/passwd", "")
	for name, e := range map[string]memory.Entry{
		"untrusted": untrusted, "superseded": superseded, "retired": retired,
		"private": private, "not an intention": notIntention, "malformed cue": malformed,
	} {
		if Fires(e, s) {
			t.Errorf("%s fired", name)
		}
	}
}

func TestSelectOrdersNewestFirstAndCapsWithACount(t *testing.T) {
	t.Parallel()
	s := Situation{Now: day("2026-10-11"), Prompt: "kestrel"}
	var notes []Located
	for i, stamp := range []string{"2026-10-01 09:00", "2026-10-05 09:00", "2026-10-03 09:00", "2026-10-02 09:00"} {
		e := intention(string(rune('a'+i)), "term:kestrel", "")
		e.Stamp = stamp
		notes = append(notes, Located{Note: "memory/n.md", Entry: e})
	}
	picked, suppressed := Select(notes, s, 3)
	if len(picked) != 3 || suppressed != 1 {
		t.Fatalf("picked %d, suppressed %d", len(picked), suppressed)
	}
	if picked[0].Entry.Stamp != "2026-10-05 09:00" || picked[2].Entry.Stamp != "2026-10-02 09:00" {
		t.Fatalf("not newest first: %v", picked)
	}
}

func TestCheckWritableRefusesUntrustedOrigins(t *testing.T) {
	t.Parallel()
	if err := CheckWritable(""); err != nil {
		t.Fatal(err)
	}
	if err := CheckWritable("connector:slack:C123"); err == nil {
		t.Fatal("an intention from a connector was accepted")
	}
}
