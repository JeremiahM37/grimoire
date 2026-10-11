package memory

import (
	"strings"
	"testing"
)

// The intention fields must round-trip byte-identically, and must not appear
// at all on a fact that has none, so every bullet written before them formats
// exactly as it did.
func TestIntentionTrailerRoundTrip(t *testing.T) {
	t.Parallel()
	e := Entry{ID: "abc123", Text: "Staging DB is shared", Agent: "claude", Stamp: "2026-10-11 09:00",
		Cue: "path:deploy/**&term:staging", Life: "until:2026-10-30", Retired: "2026-10-12T08:00:00Z"}
	line := e.Format()
	if !strings.Contains(line, " cue=path:deploy/**&term:staging") {
		t.Fatalf("cue not written: %s", line)
	}
	parsed := Parse(line)
	if len(parsed) != 1 {
		t.Fatalf("parsed %d entries from %q", len(parsed), line)
	}
	got := parsed[0]
	if got.Cue != e.Cue || got.Life != e.Life || got.Retired != e.Retired {
		t.Fatalf("intention fields lost: %+v", got)
	}
	if again := got.Format(); again != line {
		t.Fatalf("not byte-identical:\n%s\n%s", line, again)
	}
}

func TestIntentionTrailerAbsentWhenUnset(t *testing.T) {
	t.Parallel()
	e := Entry{ID: "abc123", Text: "Plain fact", Agent: "claude", Stamp: "2026-10-11 09:00"}
	line := e.Format()
	for _, key := range []string{"cue=", "life=", "ret="} {
		if strings.Contains(line, key) {
			t.Fatalf("%s written for a fact without one: %s", key, line)
		}
	}
	if want := "- **2026-10-11 09:00 \u00b7 claude** \u2014 Plain fact <!--m id=abc123-->"; line != want {
		t.Fatalf("ordinary bullet changed:\n got %s\nwant %s", line, want)
	}
}

// A cue value holds spaces and the trailer's own separators only as escapes,
// so a cue written with one must not end the trailer early.
func TestIntentionTrailerEscapesSpaces(t *testing.T) {
	t.Parallel()
	e := Entry{ID: "abc123", Text: "x", Stamp: "2026-10-11 09:00", Cue: "cmd:docker compose down"}
	line := e.Format()
	parsed := Parse(line)
	if len(parsed) != 1 || parsed[0].Cue != "cmd:docker compose down" {
		t.Fatalf("cue with a space did not survive: %q -> %+v", line, parsed)
	}
}
