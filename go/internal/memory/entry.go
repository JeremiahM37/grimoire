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
	"regexp"
	"sort"
	"strings"
	"time"
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
	// SupersededAt is when the replacement happened, in the same format as
	// Stamp. Without it "what did the agent believe last Tuesday" is
	// unanswerable: knowing a belief was eventually replaced says nothing
	// about whether it was still standing then.
	SupersededAt string

	// Line is the 0-based index of this entry's bullet in the note body. It is
	// a parse artifact, not persisted state: it exists so a rewrite can put an
	// edited entry back where it came from.
	Line int
}

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
	}
	return e, true
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
		case "immutable":
			e.Immutable = v == "1" || v == "true"
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
	if e.SupersededBy != "" {
		fields = append(fields, "sup="+escapeField(e.SupersededBy))
	}
	if e.SupersededAt != "" {
		fields = append(fields, "supat="+escapeField(e.SupersededAt))
	}
	return " <!--m " + strings.Join(fields, " ") + "-->"
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
