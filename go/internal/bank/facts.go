package bank

import (
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Fact is one extracted memory: a bullet in banks/<bank>/facts/<doc>.md.
//
//   - Alice moved to Lyon | When: May 2023 | Involving: Alice <!--f id=f1a2… sum=9c0d… type=world kind=event chunk=0 occ=2023-05-01..2023-05-31 men=2023-05-08T13:56:00Z ent=Alice;Lyon -->
//
// The trailer is machine state and the text before it is the fact. `sum` is a
// hash of the text as written by this package, which makes the file
// tamper-evident in the useful direction: a person who corrects a fact in an
// editor changes the text and not the hash, and from then on the fact is
// theirs (see Authority).
type Fact struct {
	ID   string
	Text string
	// Type is "world" (about the world and the people in it, including the
	// user's own preferences and plans) or "experience" (something the
	// assistant itself did). Observations arrive with consolidation.
	Type string
	// Kind is "event" for a datable occurrence and "conversation" for an
	// ongoing state, preference or trait.
	Kind     string
	Chunk    int // -1 when the fact is not tied to a chunk
	OccStart time.Time
	OccEnd   time.Time
	// Mentioned is when the source said it — the retained item's timestamp.
	Mentioned time.Time
	Entities  []string // canonical names, resolved at retain time
	Causes    []string // ids of facts this one was caused by
	Tags      []string

	// Human is a person's declaration of authorship, `by=human`.
	Human bool
	// HandEdited is the same claim inferred from the file: a bullet with no
	// trailer, or whose text no longer matches its sum. Never serialised —
	// it is recomputed from the evidence on every parse, and the writer turns
	// it into a declared `by=human` so the claim survives the rewrite.
	HandEdited bool
	// DocRemoved marks a human fact whose source chunk no longer exists. A
	// person's fact is never deleted because a document changed under it.
	DocRemoved bool
	// Challenges names a fact this one contradicts but may not supersede.
	// Written by consolidation (phase 2); parsed and kept now so the format
	// is stable.
	Challenges string
	// Proof is how many facts support an observation; 0 for plain facts.
	Proof int

	sum  string
	Line int // 0-based line in the file, for diagnostics
}

// Authority is the rung on the memory authority lattice this fact sits on.
// Bank facts have two: a person's, and a model's. A model never writes from
// the human rung, whatever it claims.
func (f Fact) Authority() memory.Authority {
	if f.Human || f.HandEdited {
		return memory.AuthorityHuman
	}
	return memory.AuthorityAgent
}

// IsHuman is Authority() == human.
func (f Fact) IsHuman() bool { return f.Authority() == memory.AuthorityHuman }

// textSum is the tamper-evidence hash of a fact's text.
func textSum(text string) string { return shortHash(normFactText(text), 8) }

func normFactText(s string) string { return strings.Join(strings.Fields(s), " ") }

// FactID mints the id of a model-extracted fact. It is derived rather than
// random so that re-extracting the same chunk to the same text keeps the same
// id — which keeps every reference to it valid.
func FactID(bankID, doc string, chunk, idx int, text string) string {
	return "f" + shortHash(bankID+"\x00"+doc+"\x00"+itoa(chunk)+"\x00"+itoa(idx)+"\x00"+normFactText(text), 15)
}

// handID is the id of a bullet a person typed with no trailer: derived from
// where it is and what it says, so a reindex finds the same id again.
func handID(bankID, doc, text string) string {
	return "h" + shortHash(bankID+"\x00"+doc+"\x00"+normFactText(text), 15)
}

// fieldEscaper keeps a trailer value from containing the characters the
// trailer itself is built from.
var fieldEscaper = strings.NewReplacer("%", "%25", " ", "%20", ";", "%3B", ",", "%2C",
	"=", "%3D", ">", "%3E", "\n", "%0A", "\r", "%0D")
var fieldUnescaper = strings.NewReplacer("%25", "%", "%20", " ", "%3B", ";", "%2C", ",",
	"%3D", "=", "%3E", ">", "%0A", "\n", "%0D", "\r")

func escapeField(s string) string   { return fieldEscaper.Replace(s) }
func unescapeField(s string) string { return fieldUnescaper.Replace(s) }

func joinComma(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = escapeField(x)
	}
	return strings.Join(out, ",")
}

func splitComma(s string) []string { return splitOn(s, ",") }

func splitOn(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, sep) {
		if p = unescapeField(p); strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func trailerFields(s string) map[string]string {
	out := map[string]string{}
	for _, tok := range strings.Fields(s) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			out[tok] = "1" // a bare flag
			continue
		}
		out[k] = v
	}
	return out
}

const timeLayout = "2006-01-02T15:04:05Z07:00"

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	t = t.UTC()
	if t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 && t.Nanosecond() == 0 {
		return t.Format("2006-01-02")
	}
	return t.Format(timeLayout)
}

func parseTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, timeLayout, "2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// Format renders the fact as one bullet line.
func (f Fact) Format() string {
	text := normFactText(f.Text)
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(text)
	b.WriteString(" <!--f id=" + escapeField(f.ID))
	// A fact a person edited is written back as declared human, so the claim
	// does not depend on the sum continuing to mismatch.
	if f.Human || f.HandEdited {
		b.WriteString(" by=human")
	}
	b.WriteString(" sum=" + textSum(text))
	if f.Type != "" {
		b.WriteString(" type=" + f.Type)
	}
	if f.Kind != "" {
		b.WriteString(" kind=" + f.Kind)
	}
	if f.Chunk >= 0 {
		b.WriteString(" chunk=" + itoa(f.Chunk))
	}
	if !f.OccStart.IsZero() {
		end := f.OccEnd
		if end.IsZero() {
			end = f.OccStart
		}
		b.WriteString(" occ=" + fmtTime(f.OccStart) + ".." + fmtTime(end))
	}
	if !f.Mentioned.IsZero() {
		b.WriteString(" men=" + fmtTime(f.Mentioned))
	}
	if len(f.Entities) > 0 {
		parts := make([]string, len(f.Entities))
		for i, e := range f.Entities {
			parts[i] = escapeField(e)
		}
		b.WriteString(" ent=" + strings.Join(parts, ";"))
	}
	if len(f.Causes) > 0 {
		b.WriteString(" cause=" + joinComma(f.Causes))
	}
	if len(f.Tags) > 0 {
		b.WriteString(" tags=" + joinComma(f.Tags))
	}
	if f.Proof > 0 {
		b.WriteString(" proof=" + itoa(f.Proof))
	}
	if f.Challenges != "" {
		b.WriteString(" chal=" + escapeField(f.Challenges))
	}
	if f.DocRemoved {
		b.WriteString(" doc_removed")
	}
	b.WriteString(" -->")
	return b.String()
}

// ParseFactLine reads one bullet. ok is false for a line that is not a bullet.
// bankID and doc are needed only to derive an id for a hand-typed bullet.
func ParseFactLine(line, bankID, doc string) (Fact, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "- ") && !strings.HasPrefix(s, "* ") {
		return Fact{}, false
	}
	s = strings.TrimSpace(s[2:])
	f := Fact{Chunk: -1, Type: "world", Kind: "conversation"}
	i := strings.LastIndex(s, "<!--f ")
	if i < 0 || !strings.HasSuffix(s, "-->") {
		// No trailer: a person wrote this bullet.
		f.Text = normFactText(s)
		if f.Text == "" {
			return Fact{}, false
		}
		f.HandEdited = true
		f.ID = handID(bankID, doc, f.Text)
		return f, true
	}
	f.Text = normFactText(s[:i])
	if f.Text == "" {
		return Fact{}, false
	}
	fields := trailerFields(strings.TrimSuffix(s[i+len("<!--f "):], "-->"))
	for k, v := range fields {
		switch k {
		case "id":
			f.ID = unescapeField(v)
		case "sum":
			f.sum = v
		case "by":
			f.Human = v == "human"
		case "type":
			f.Type = v
		case "kind":
			f.Kind = v
		case "chunk":
			if n, err := atoi(v); err == nil {
				f.Chunk = n
			}
		case "occ":
			a, b, _ := strings.Cut(v, "..")
			f.OccStart, f.OccEnd = parseTime(a), parseTime(b)
			if f.OccEnd.IsZero() {
				f.OccEnd = f.OccStart
			}
		case "men":
			f.Mentioned = parseTime(v)
		case "ent":
			f.Entities = splitOn(v, ";")
		case "cause":
			f.Causes = splitComma(v)
		case "tags":
			f.Tags = splitComma(v)
		case "proof":
			f.Proof, _ = atoi(v)
		case "chal":
			f.Challenges = unescapeField(v)
		case "doc_removed":
			f.DocRemoved = true
		}
	}
	if f.ID == "" {
		f.ID = handID(bankID, doc, f.Text)
		f.HandEdited = true
	}
	// The sum is checked against the text as it now reads. A missing sum is
	// as good as a wrong one: only this package writes trailers with one.
	if f.sum != textSum(f.Text) {
		f.HandEdited = true
	}
	return f, true
}

// FactsFile is the parsed content of a facts file.
type FactsFile struct {
	Facts []Fact
	// Prose is every non-bullet line a person added, kept verbatim and in
	// order so a rewrite does not lose what they wrote between the bullets.
	Prose []string
}

// ParseFacts reads a facts file body.
func ParseFacts(body, bankID, doc string) FactsFile {
	var out FactsFile
	seen := map[string]int{}
	for n, ln := range strings.Split(body, "\n") {
		if f, ok := ParseFactLine(ln, bankID, doc); ok {
			f.Line = n
			// Two hand-typed bullets with identical text derive the same id;
			// suffix the later one so both stay addressable.
			if c := seen[f.ID]; c > 0 {
				f.ID = f.ID + "-" + itoa(c)
			}
			seen[f.ID]++
			out.Facts = append(out.Facts, f)
			continue
		}
		t := strings.TrimSpace(ln)
		if t == "" || strings.HasPrefix(t, "# Facts:") {
			continue
		}
		out.Prose = append(out.Prose, ln)
	}
	return out
}

// FormatFacts renders a facts file body.
func FormatFacts(doc string, ff FactsFile) string {
	var b strings.Builder
	b.WriteString("# Facts: " + oneLine(doc) + "\n\n")
	for _, f := range ff.Facts {
		b.WriteString(f.Format())
		b.WriteByte('\n')
	}
	if len(ff.Prose) > 0 {
		b.WriteByte('\n')
		for _, p := range ff.Prose {
			b.WriteString(p)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// eventTime is the instant a fact is placed at in time: when it happened if
// known, else when it was said.
func (f Fact) eventTime() time.Time {
	if !f.OccStart.IsZero() {
		return f.OccStart
	}
	return f.Mentioned
}

// EmbedText is what the embedder sees for a fact: the text, the month it
// happened in, and the entities, so a query about "Alice in May" can match on
// meaning, period and name at once without the stored text carrying either.
func (f Fact) EmbedText() string {
	s := f.Text
	if d := f.eventTime(); !d.IsZero() {
		if !f.OccStart.IsZero() && !f.OccEnd.IsZero() && !sameMonth(f.OccStart, f.OccEnd) {
			s += " (happened from " + d.Format("January 2006") + " to " + f.OccEnd.Format("January 2006") + ")"
		} else {
			s += " (happened in " + d.Format("January 2006") + ")"
		}
	}
	if len(f.Entities) > 0 {
		s += " [" + strings.Join(f.Entities, ", ") + "]"
	}
	return s
}

func sameMonth(a, b time.Time) bool { return a.Year() == b.Year() && a.Month() == b.Month() }

func itoa(n int) string { return strconv.Itoa(n) }

func atoi(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }
