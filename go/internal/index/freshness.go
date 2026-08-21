package index

import (
	"math"
	"os"
	"strconv"
	"time"
)

// How old a passage is, and whether that should worry the reader.
//
// Agent memory already models time properly: facts decay in ranking with a
// 90-day half-life, carry a TTL, and can be reconstructed as-of an instant.
// Knowledge notes had none of it. A runbook nobody has touched in eighteen
// months is retrieved, cited and answered from with exactly the confidence of
// one written yesterday — and the failure that produces is the quiet kind. The
// agent is not wrong about what the note says. The note is wrong.
//
// Two decisions here, both deliberate:
//
//   - Age is EXPOSED, not acted on. It would be easy to decay note ranking the
//     way memory ranking decays, and it would be wrong: a memory fact is a
//     claim about a changing world, while a note might be a decision record, a
//     book summary or a poem, none of which get less true. Down-ranking old
//     notes would bury the vault's most considered writing under its most
//     recent. So retrieval reports the age and the caller decides — the same
//     argument that made Hit expose cosine and lexical instead of only a rank.
//
//   - `verified:` beats `updated`. A note's modification time answers "when
//     was this touched", which a typo fix bumps. The question is "when did
//     somebody last confirm this is still true", and only a person can answer
//     it, so there is a frontmatter key for saying so.

// DefaultStaleAfter is when a note starts being reported as stale.
//
// Six months, because that is roughly the interval over which infrastructure
// documentation in practice stops being right: hosts move, versions bump,
// people leave. It is a default rather than a truth, and one env var moves it.
const DefaultStaleAfter = 180 * 24 * time.Hour

// StaleAfter reads the configured staleness threshold. 0 disables the signal
// entirely, for a vault of writing that does not go stale.
func StaleAfter() time.Duration {
	if v := os.Getenv("GRIMOIRE_STALE_AFTER_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * 24 * time.Hour
		}
	}
	return DefaultStaleAfter
}

// Freshness builds the exported view of a note's age, for callers outside the
// package that hold the same two columns.
func Freshness(verified string, mtime float64) Fresh {
	return Fresh{f: freshness{verified: verified, mtime: mtime}}
}

// Fresh is a note's freshness, as seen from outside the package.
type Fresh struct{ f freshness }

// AgeDays is days since the note was last confirmed, and whether that
// confirmation was explicit.
func (x Fresh) AgeDays(now time.Time) (int, bool) { return x.f.ageDays(now) }

// ValidVerifiedDate reports whether a string is a `verified:` value this
// package can actually read back. A date the parser refuses would leave a note
// carrying the key and still counted as never checked.
func ValidVerifiedDate(s string) bool {
	_, ok := parseVerified(s)
	return ok
}

// freshness is a note's age and where the number came from.
type freshness struct {
	// verified is the frontmatter date, empty when nobody has vouched.
	verified string
	// mtime is the file's modification time, always available.
	mtime float64
}

// ageDays returns whole days since the note was last CONFIRMED, and whether
// that confirmation was an explicit `verified:` date rather than a file write.
//
// A `verified:` date in the future is ignored rather than clamped to zero: it
// is a typo (2027 for 2026 is the classic), and honouring it would make the
// one note somebody fat-fingered permanently the freshest thing in the vault.
func (f freshness) ageDays(now time.Time) (days int, explicit bool) {
	if t, ok := parseVerified(f.verified); ok && !t.After(now) {
		return int(now.Sub(t).Hours() / 24), true
	}
	if f.mtime <= 0 {
		return 0, false
	}
	mt := time.Unix(int64(f.mtime), 0)
	if mt.After(now) {
		return 0, false
	}
	return int(now.Sub(mt).Hours() / 24), false
}

// parseVerified accepts the shapes a person actually types into frontmatter.
func parseVerified(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02 15:04", "2006-01-02", "2006-01"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// StalenessScore ranks a note for a review queue: how overdue it is, weighted
// by how much the vault leans on it.
//
// Inbound links are the weight because they are ALREADY IN THE INDEX. The
// obvious weight would be "how often is this retrieved", which would mean
// counting every retrieval of every note forever — a new collector, new
// storage, and a privacy surface (what people search for) that this project
// deliberately does not build; the read audit refuses to log search for the
// same reason. Inbound links measure something slightly different and better:
// not what got asked for, but what the rest of the vault depends on. A runbook
// twelve notes point at, last confirmed a year ago, is exactly the top of the
// list you want.
//
// log1p on the link count, so a hub with 200 backlinks does not drown out
// twenty genuinely rotten notes with three each.
func StalenessScore(ageDays, inbound int) float64 {
	if ageDays <= 0 {
		return 0
	}
	return float64(ageDays) * (1 + math.Log1p(float64(inbound)))
}
