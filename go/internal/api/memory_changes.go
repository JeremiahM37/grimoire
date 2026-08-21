package api

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// GET /api/memory/changes — what your agents learned, and what they changed
// their mind about.
//
// Grimoire already keeps the one thing that makes this answerable and that no
// other memory layer keeps: a superseded fact is struck through rather than
// deleted, with the time it happened. `as_of` uses that to reconstruct a past
// belief state. But nothing ever read it FORWARD — and the forward question is
// the one a person actually has: not "what did it believe in June", which
// nobody wonders, but "what changed this week", which is how you notice that an
// agent quietly replaced a correct fact with a wrong one.
//
// It is a DIFF, not a listing. A recall sorted by recency answers "what is
// believed"; this answers "what moved", which is a different set: a fact
// written a year ago and superseded yesterday belongs in this week's digest
// and in no recency listing.

// changeKind is what happened to a belief in the window.
const (
	changeLearned   = "learned"   // a new fact, replacing nothing
	changeChanged   = "changed"   // a belief was replaced by another
	changeRetracted = "retracted" // a belief was withdrawn, with no replacement
	changeExpired   = "expired"   // a belief's own time-to-live ran out
)

type beliefChange struct {
	Kind string `json:"kind"`
	At   string `json:"at"`

	// The fact as it now stands. For a retraction or an expiry this is the
	// fact that STOPPED being believed, since there is no successor.
	ID    string `json:"id"`
	Text  string `json:"text"`
	Path  string `json:"path"`
	Agent string `json:"agent,omitempty"`
	Topic string `json:"topic,omitempty"`

	// Replaced is the belief this one superseded — the whole point of the
	// "changed" row. Without the old text a digest says "something changed"
	// and leaves the reader to go and look, which is the same as not saying it.
	Replaced     string `json:"replaced,omitempty"`
	ReplacedID   string `json:"replaced_id,omitempty"`
	ReplacedText string `json:"replaced_text,omitempty"`

	Origin string `json:"origin,omitempty"`
	Trust  string `json:"trust"`
}

// changeScanLimit bounds how many entries a digest considers. Ranking runs
// over everything that matched and truncates at the end, so this is an output
// bound rather than a scan bound; it exists so a vault with a hundred thousand
// facts cannot turn one request into an unbounded response.
const changeScanLimit = 2000

// defaultChangeWindow is a week: the digest is meant to be read on a rhythm,
// and a window shorter than the gap between readings hides exactly the changes
// somebody came to look for.
const defaultChangeWindow = 7 * 24 * time.Hour

func (s *Server) memoryChanges(w http.ResponseWriter, r *http.Request) {
	now := vault.Now()
	since, err := parseSince(r.URL.Query().Get("since"), now)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 100, 500)

	// Superseded AND expired entries are included deliberately: they are the
	// subject. A digest built from live facts alone could report what was
	// learned and never what was lost, which is the half that matters.
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            filterFor(r, true),
		Agent:             strings.TrimSpace(r.URL.Query().Get("agent")),
		Session:           strings.TrimSpace(r.URL.Query().Get("session")),
		IncludeSuperseded: true,
		IncludeExpired:    true,
		Now:               now,
		// An explicit, generous scan limit. MemoryEntries treats Limit <= 0 as
		// "use the default of 20", so passing 0 to mean "no limit" would
		// silently truncate the digest to twenty entries and report counts
		// that were wrong on any busy week — the failure would look like a
		// quiet week rather than like a bug.
		Limit: changeScanLimit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	byID := make(map[string]index.MemoryHit, len(hits))
	// successors are facts that replaced something. They are reported on the
	// REPLACED fact's row, which carries both texts — so listing them again as
	// "learned" would report one correction as two events, and inflate every
	// count a person reads off the top of the digest. One event, one row.
	successors := make(map[string]bool, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
		if h.Superseded() && !strings.HasPrefix(h.SupersededBy, "retracted:") {
			successors[h.SupersededBy] = true
		}
	}

	out := []beliefChange{}
	for _, h := range hits {
		if h.Superseded() {
			// The successor's row is where a replacement is reported, so the
			// old fact is not also listed as "learned" — one event, one row.
			at, ok := stampTime(h.SupersededAt)
			if !ok || at.Before(since) {
				continue
			}
			if strings.HasPrefix(h.SupersededBy, "retracted:") {
				c := changeFrom(changeRetracted, h.SupersededAt, h)
				c.Agent = strings.TrimPrefix(h.SupersededBy, "retracted:")
				out = append(out, c)
				continue
			}
			successor, known := byID[h.SupersededBy]
			if !known {
				// The successor is gone — a hand-edited note, or a fact whose
				// note was deleted. Reporting the disappearance is still more
				// useful than silence, and claiming a replacement whose text
				// cannot be shown would be worse.
				c := changeFrom(changeRetracted, h.SupersededAt, h)
				out = append(out, c)
				continue
			}
			c := changeFrom(changeChanged, h.SupersededAt, successor)
			c.Replaced, c.ReplacedID, c.ReplacedText = h.Text, h.ID, h.Text
			out = append(out, c)
			continue
		}
		// A live fact that expired inside the window: believed, then not,
		// without anybody writing anything.
		if h.Expires != "" {
			if exp, err := time.Parse(time.RFC3339, h.Expires); err == nil &&
				!exp.After(now) && !exp.Before(since) {
				out = append(out, changeFrom(changeExpired, exp.Format(memory.StampFormat), h))
				continue
			}
		}
		if successors[h.ID] {
			continue
		}
		at, ok := stampTime(h.Stamp)
		if !ok || at.Before(since) {
			continue
		}
		out = append(out, changeFrom(changeLearned, h.Stamp, h))
	}

	// Newest first. A digest is read from the top and abandoned partway down,
	// so the ordering decides what actually gets read.
	sort.SliceStable(out, func(i, j int) bool { return out[i].At > out[j].At })
	if len(out) > limit {
		out = out[:limit]
	}

	counts := map[string]int{changeLearned: 0, changeChanged: 0,
		changeRetracted: 0, changeExpired: 0}
	for _, c := range out {
		counts[c.Kind]++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"since":   since.Format(time.RFC3339),
		"changes": out,
		"counts":  counts,
	})
}

func changeFrom(kind, at string, h index.MemoryHit) beliefChange {
	return beliefChange{
		Kind: kind, At: at, ID: h.ID, Text: h.Text, Path: h.Note,
		Agent: h.Agent, Topic: topicOf(h.Note),
		Origin: h.Origin, Trust: trust.FromOrigin(h.Origin).String(),
	}
}

// topicOf recovers the memory topic from a note path, which is how a person
// names these ("what changed about deploy?") rather than by file.
func topicOf(path string) string {
	base := path
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[i+1:]
	}
	return strings.TrimSuffix(base, ".md")
}

// stampTime parses a bullet timestamp. Bullets carry local wall-clock time, so
// the comparison happens in local time too — the same rule Entry.BelievedAt
// follows, and disagreeing with it would put a fact in one window here and a
// different one there.
func stampTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(memory.StampFormat, s, time.Local); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// parseSince accepts an RFC3339 instant or a duration back from now ("7d",
// "24h", "90m").
//
// A bad value is an ERROR rather than a silent fallback to the default window.
// A digest quietly answering about a different period than the one asked for
// is a wrong answer that looks right — the same reasoning as recall's as_of.
func parseSince(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return now.Add(-defaultChangeWindow), nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	if d, ok := parseWindow(raw); ok {
		return now.Add(-d), nil
	}
	return time.Time{}, errBadSince
}

var errBadSince = &sinceError{}

type sinceError struct{}

func (*sinceError) Error() string {
	return "since must be an RFC3339 instant or a duration like 7d, 24h, 90m"
}

// parseWindow understands Go durations plus "d" for days, which time.ParseDuration
// does not and which is the unit a person asking for a digest thinks in.
func parseWindow(raw string) (time.Duration, bool) {
	if strings.HasSuffix(raw, "d") {
		days, err := time.ParseDuration(strings.TrimSuffix(raw, "d") + "h")
		if err != nil || days <= 0 {
			return 0, false
		}
		return days * 24, true
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// recentBeliefChanges counts what moved in the default window, for the
// briefing. Counts rather than rows: a briefing is read before work starts and
// has to stay small enough that it is actually read.
func (s *Server) recentBeliefChanges(r *http.Request) (changed, retracted int) {
	now := vault.Now()
	since := now.Add(-defaultChangeWindow)
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, false), IncludeSuperseded: true, IncludeExpired: true,
		Now: now, Limit: changeScanLimit,
	})
	if err != nil {
		// A briefing that fails because a count could not be taken is worse
		// than one reporting zero: the counts are context, not the answer.
		return 0, 0
	}
	for _, h := range hits {
		if !h.Superseded() {
			continue
		}
		at, ok := stampTime(h.SupersededAt)
		if !ok || at.Before(since) {
			continue
		}
		if strings.HasPrefix(h.SupersededBy, "retracted:") {
			retracted++
			continue
		}
		changed++
	}
	return changed, retracted
}
