// Package memory is the fact-level model of agent memory.
//
// A memory note is a markdown file of bullet lines, and each bullet is one
// fact. Everything the memory engine does — reconciling a new fact against
// what is already known, boosting recall by entity, expiring a fact, showing
// that one fact replaced another — needs to address a single bullet rather
// than the note it lives in. This package is the parse/format boundary that
// makes a bullet addressable while leaving the markdown as the source of
// truth.
//
// The markdown stays the source of truth deliberately. Every competing memory
// layer stores facts as rows in a vector store, which makes "what does my
// agent believe, and why" a database query nobody runs. Here it is a file you
// open, diff, and roll back — so the round trip through this package has to be
// lossless, which is what entry_test.go is mostly about.
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// Dir is the vault namespace agent memory lives under.
const Dir = "memory"

// Entry is one remembered fact.
type Entry struct {
	ID   string // stable across rewrites; minted on first write
	Text string // the fact itself

	// Provenance — who wrote it, doing what, when.
	Agent   string
	Task    string
	Session string // mem0's run_id: the task/conversation this was learned in
	Stamp   string // display timestamp, "2006-01-02 15:04"

	// Lifecycle.
	Category     string // free-form bucket: preference, fact, procedure, …
	Expires      string // RFC3339; empty means never
	Immutable    bool   // reconciliation may not supersede or delete it
	SupersededBy string // ID of the entry that replaced this one

	// Challenges is the ID of an entry this fact contradicts but was not
	// allowed to supersede, because that entry outranks this one's writer. It
	// rides in the bullet like everything else, so an open disagreement is
	// visible in the file rather than only in an API response nobody reads.
	Challenges string

	// Human records that a PERSON asserted this, declared by `by=human` in the
	// trailer. It is the top rung of the authority lattice: an agent write may
	// not supersede it. See authority.go.
	Human bool

	// HandWritten is the same claim, INFERRED rather than declared, and it is
	// never serialised — it is recomputed on every parse, because the evidence
	// for it is the file itself.
	//
	// A bullet nothing in this package wrote is recognisable two ways: it has
	// no trailer at all, or its id no longer matches its own content. The id is
	// a hash of (stamp, agent, text), so text that changed after the id was
	// minted is text some other hand changed. That makes the on-disk format
	// tamper-evident for free, and it is the only reason a store like this one
	// can tell a person's correction from an agent's write at all — a vector
	// database has nowhere to put the discrepancy.
	HandWritten bool
	// Helpful and Unhelpful count the times a caller reported this fact as
	// having earned its place in a recall, or not. They ride in the bullet
	// like everything else, so the signal is as rebuildable — and as visible —
	// as the fact it is about.
	Helpful   int
	Unhelpful int

	// SupersededAt is when the replacement happened, in the same format as
	// Stamp. Without it "what did the agent believe last Tuesday" is
	// unanswerable: knowing a belief was eventually replaced says nothing
	// about whether it was still standing then.
	SupersededAt string

	// Origin is where the fact CAME FROM, when the agent knew: a note it read,
	// a connector document, a web page. Empty means the agent asserted it
	// itself, which is the ordinary case and the trusted one.
	//
	// This is not the same as Agent, which says WHO wrote it. An agent the
	// operator runs is trusted; a sentence that agent copied out of a Jira
	// comment is not, and the difference is exactly what an injected "remember
	// that the deploy key is X" exploits.
	Origin string

	// Line is the 0-based index of this entry's bullet in the note body. It is
	// a parse artifact, not persisted state: it exists so a rewrite can put an
	// edited entry back where it came from.
	Line int
}

// Untrusted reports whether this fact came from text other people can write.
func (e Entry) Untrusted() bool { return trust.FromOrigin(e.Origin) == trust.Untrusted }

// Superseded reports whether a later fact replaced this one. Superseded
// entries stay in the note — struck through rather than deleted — because the
// audit trail of what an agent used to believe is the reason this is a file
// and not a row.
func (e Entry) Superseded() bool { return e.SupersededBy != "" }

// ExpiredAt reports whether the entry's time-to-live has run out.
func (e Entry) ExpiredAt(now time.Time) bool {
	if e.Expires == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, e.Expires)
	if err != nil {
		return false
	}
	return now.After(t)
}

// Live reports whether recall should return this entry by default.
func (e Entry) Live(now time.Time) bool { return !e.Superseded() && !e.ExpiredAt(now) }

// StampFormat is how a bullet's timestamps are written. Minute resolution:
// enough to order an agent's writes, short enough to stay readable in a line a
// person is meant to be able to scan.
const StampFormat = "2006-01-02 15:04"

// BelievedAt reports whether this fact was a current belief at an instant —
// written by then, not yet replaced, and not yet expired.
//
// A fact whose supersession has no recorded time is treated as having been
// replaced from the start. That is the conservative direction: it can only
// hide a fact from a historical view, never resurrect one the agent has
// stopped believing.
func (e Entry) BelievedAt(t time.Time) bool {
	if written, ok := parseStamp(e.Stamp); ok && written.After(t) {
		return false
	}
	if e.Superseded() {
		replaced, ok := parseStamp(e.SupersededAt)
		if !ok || !replaced.After(t) {
			return false
		}
	}
	return !e.ExpiredAt(t)
}

// parseStamp reads a bullet timestamp. Bullets carry local wall-clock time,
// which is what a person reading the note expects to see; comparisons against
// it therefore happen in local time too.
func parseStamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.ParseInLocation(StampFormat, s, time.Local); err == nil {
		return t, true
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// The bullet grammar. Attribution is non-greedy so a fact containing the
// separator does not eat it; the trailer is stripped before the split, so a
// fact may contain anything an HTML comment may not.
var (
	bulletRE  = regexp.MustCompile(`^-\s+(~~)?\*\*(.+?)\*\*\s+—\s+(.*)$`)
	trailerRE = regexp.MustCompile(`\s*<!--m\s+([^>]*?)\s*-->\s*$`)
	strikeRE  = regexp.MustCompile(`~~\s*$`)
)

// Parse reads the entries out of a memory note body. Lines that are not
// bullets — the heading, blank lines, anything a person typed — are not
// entries and are preserved by Rewrite rather than represented here.
func Parse(body string) []Entry {
	var out []Entry
	for i, line := range strings.Split(body, "\n") {
		e, ok := ParseLine(line)
		if !ok {
			continue
		}
		e.Line = i
		out = append(out, e)
	}
	return out
}

// ParseLine reads one bullet. It reports false for anything that is not one.
func ParseLine(line string) (Entry, bool) {
	m := bulletRE.FindStringSubmatch(strings.TrimRight(line, " \t"))
	if m == nil {
		return Entry{}, false
	}
	struck := m[1] == "~~"
	attribution, rest := m[2], m[3]

	var e Entry
	if t := trailerRE.FindStringSubmatch(rest); t != nil {
		e = parseTrailer(t[1])
		rest = rest[:len(rest)-len(t[0])]
	}
	if struck {
		rest = strikeRE.ReplaceAllString(strings.TrimRight(rest, " \t"), "")
	}
	e.Text = strings.TrimSpace(rest)

	// "2026-08-14 09:00 · agent · task" — the timestamp is always first, the
	// agent always second, and anything after that is the task (which may
	// itself contain the separator).
	parts := strings.Split(attribution, " · ")
	if len(parts) > 0 {
		e.Stamp = strings.TrimSpace(parts[0])
	}
	if len(parts) > 1 {
		e.Agent = strings.TrimSpace(parts[1])
	}
	if len(parts) > 2 {
		e.Task = strings.TrimSpace(strings.Join(parts[2:], " · "))
	}
	// A bullet written before this package existed has no trailer and so no
	// id. Deriving one from its content keeps it addressable — it can be
	// superseded, expired and recalled like any other — and the id becomes
	// permanent the first time the line is rewritten.
	if e.ID == "" {
		e.ID = DeriveID(e.Stamp, e.Agent, e.Text)
		// No id in the file means no writer in this package produced this
		// line: a person typed the bullet themselves.
		e.HandWritten = true
	} else if looksMinted(e.ID) && !e.idMatchesContent() {
		// An id that no longer describes its own text is the signature of an
		// edit made outside the write path — which is exactly the correction
		// the file format exists to allow.
		e.HandWritten = true
	}
	return e, true
}

// mintedRE matches an id this package could have produced: twelve lowercase hex
// characters, optionally with a collision suffix.
var mintedRE = regexp.MustCompile(`^[0-9a-f]{12}(-[0-9]+)?$`)

// looksMinted reports whether an id is in the format DeriveID produces.
//
// The hand-edit inference is only sound for ids this package minted. An id in
// any other shape — one a test fixture invented, or one a future scheme
// produces — carries no claim about its own content, so comparing it to a hash
// proves nothing and must not be read as evidence that a person edited the line.
func looksMinted(id string) bool { return mintedRE.MatchString(id) }

// idMatchesContent reports whether this entry's id is still the hash of its own
// (stamp, agent, text).
//
// The collision suffix is stripped first: two facts remembered in the same
// minute by the same agent with the same text mint the same id, and the second
// gets "-1" appended. That suffix says nothing about whether the text changed.
func (e Entry) idMatchesContent() bool {
	base := e.ID
	if i := strings.LastIndex(base, "-"); i > 0 {
		if _, err := strconv.Atoi(base[i+1:]); err == nil {
			base = base[:i]
		}
	}
	return base == DeriveID(e.Stamp, e.Agent, e.Text)
}

func parseTrailer(s string) Entry {
	var e Entry
	for _, field := range strings.Fields(s) {
		k, v, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		v = unescapeField(v)
		switch k {
		case "id":
			e.ID = v
		case "session":
			e.Session = v
		case "cat":
			e.Category = v
		case "exp":
			e.Expires = v
		case "sup":
			e.SupersededBy = v
		case "supat":
			e.SupersededAt = v
		case "up":
			e.Helpful = atoiSafe(v)
		case "down":
			e.Unhelpful = atoiSafe(v)
		case "immutable":
			e.Immutable = v == "1" || v == "true"
		case "org":
			e.Origin = v
		case "by":
			e.Human = v == "human"
		case "chal":
			e.Challenges = v
		}
	}
	return e
}

// Trailer fields are space-separated, so a value containing a space would end
// the field early and silently drop the rest. Escaping is cheaper than a
// quoting grammar and round-trips exactly.
var (
	fieldEscaper   = strings.NewReplacer(" ", "\\s", "\\", "\\\\", ">", "\\g")
	fieldUnescaper = strings.NewReplacer("\\s", " ", "\\\\", "\\", "\\g", ">")
)

func escapeField(s string) string   { return fieldEscaper.Replace(s) }
func unescapeField(s string) string { return fieldUnescaper.Replace(s) }

// Format renders an entry as the bullet line it is stored as. Format(Parse(x))
// is x for any bullet this package wrote.
func (e Entry) Format() string {
	attribution := e.Stamp
	if e.Agent != "" {
		attribution += " · " + e.Agent
	}
	if e.Task != "" {
		attribution += " · " + e.Task
	}
	text := e.Text
	body := "**" + attribution + "** — " + text
	if e.Superseded() {
		// Struck through so the console shows at a glance which beliefs are
		// current without anyone having to read the trailer.
		body = "~~" + body + "~~"
	}
	return "- " + body + e.trailer()
}

func (e Entry) trailer() string {
	fields := []string{"id=" + e.ID}
	if e.Session != "" {
		fields = append(fields, "session="+escapeField(e.Session))
	}
	if e.Category != "" {
		fields = append(fields, "cat="+escapeField(e.Category))
	}
	if e.Expires != "" {
		fields = append(fields, "exp="+escapeField(e.Expires))
	}
	if e.Immutable {
		fields = append(fields, "immutable=1")
	}
	if e.Origin != "" {
		fields = append(fields, "org="+escapeField(e.Origin))
	}
	if e.Challenges != "" {
		fields = append(fields, "chal="+escapeField(e.Challenges))
	}
	if e.Human || e.HandWritten {
		// Written for BOTH, so an inferred hand-edit becomes declared the
		// first time the line is rewritten. Otherwise the inference has to
		// hold forever against a file the agent keeps touching, and the one
		// rewrite that re-mints the id would silently demote a person's
		// correction back to an agent's.
		fields = append(fields, "by=human")
	}
	if e.SupersededBy != "" {
		fields = append(fields, "sup="+escapeField(e.SupersededBy))
	}
	if e.Helpful > 0 {
		fields = append(fields, "up="+strconv.Itoa(e.Helpful))
	}
	if e.Unhelpful > 0 {
		fields = append(fields, "down="+strconv.Itoa(e.Unhelpful))
	}
	if e.SupersededAt != "" {
		fields = append(fields, "supat="+escapeField(e.SupersededAt))
	}
	return " <!--m " + strings.Join(fields, " ") + "-->"
}

// atoiSafe reads a trailer counter. A malformed one is zero rather than an
// error: a hand-edited bullet must still parse, since hand-editing is the
// point of storing memory in a file.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Usefulness scores the feedback a fact has collected, 0..1, neutral at 0.5.
//
// Bounded and saturating on purpose. Feedback is a nudge in ranking, not a
// verdict: a fact nobody has voted on must not be handicapped against one with
// a single upvote, and a fact somebody dislikes must sink rather than vanish —
// it can still be the only fact that answers a question.
func (e Entry) Usefulness() float64 {
	net := float64(e.Helpful - e.Unhelpful)
	return 0.5 + 0.5*math.Tanh(net/3)
}

// DeriveID mints the id for a fact. It is a content hash rather than a
// counter or a random string so that a vault indexed twice — or indexed on two
// devices that later sync — agrees with itself about which bullet is which.
func DeriveID(stamp, agent, text string) string {
	sum := sha256.Sum256([]byte(stamp + "\x00" + agent + "\x00" + Normalize(text)))
	return hex.EncodeToString(sum[:])[:12]
}

// Rewrite puts edited entries back into a note body, matching them to the
// lines they were parsed from. Lines that are not entries are untouched, so a
// person's own headings, prose and blank lines survive an agent's write.
func Rewrite(body string, edited []Entry) string {
	byLine := make(map[int]Entry, len(edited))
	for _, e := range edited {
		byLine[e.Line] = e
	}
	lines := strings.Split(body, "\n")
	for i := range lines {
		if e, ok := byLine[i]; ok {
			lines[i] = e.Format()
		}
	}
	return strings.Join(lines, "\n")
}

// Append adds a new entry to the end of a note body, after the last bullet if
// there is one so that a trailing prose footer stays a footer.
func Append(body string, e Entry) string {
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	last := -1
	for i, line := range lines {
		if _, ok := ParseLine(line); ok {
			last = i
		}
	}
	formatted := e.Format()
	if last < 0 {
		return strings.TrimRight(body, "\n") + "\n" + formatted + "\n"
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, lines[:last+1]...)
	out = append(out, formatted)
	out = append(out, lines[last+1:]...)
	return strings.Join(out, "\n") + "\n"
}

// SortByRecency orders entries newest first, breaking ties on id so the order
// is total and a test can rely on it.
func SortByRecency(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Stamp != entries[j].Stamp {
			return entries[i].Stamp > entries[j].Stamp
		}
		return entries[i].ID < entries[j].ID
	})
}
