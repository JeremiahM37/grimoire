package bank

import (
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Observation is a durable piece of knowledge consolidated from several facts:
// a bullet in banks/<bank>/observations.md.
//
//   - Alice lives in Lyon and works nights at Mercy Hospital <!--o id=o3f… sum=1a2b3c4d src=f1…,f2… proof=2 ev=f1…:moved%20to%20Lyon tags=family -->
//
// Like a fact, the text before the trailer is the observation and the trailer
// is machine state; `sum` makes a person's edit detectable, and an edited or
// hand-typed observation is the person's from then on. A model never updates
// or deletes a person's observation — it files a challenge beside it.
//
// When a model revises or retires one of its own observations, the old text
// moves to the file's "## History" section, struck through, so what the bank
// used to believe stays readable.
type Observation struct {
	ID   string
	Text string
	// Sources are the facts the observation was built from — its evidence.
	Sources []string
	// Evidence holds the short quotes the model cited from its sources.
	Evidence   []Evidence
	Tags       []string
	OccStart   time.Time
	OccEnd     time.Time
	Mentioned  time.Time
	Human      bool
	HandEdited bool
	// Challenges is set on a model observation that disputes a person's:
	// the id of the human observation it would have changed.
	Challenges string
	Updated    time.Time

	// History bullets: Of is the observation this text was a version of, At
	// when it was replaced, and Deleted marks a retirement rather than a
	// revision.
	Of      string
	At      time.Time
	Deleted bool

	sum  string
	Line int
}

// Evidence is one cited source of an observation.
type Evidence struct {
	FactID string `json:"fact_id"`
	Quote  string `json:"quote,omitempty"`
}

// Authority is the observation's rung: a person's or a model's.
func (o Observation) Authority() memory.Authority {
	if o.Human || o.HandEdited {
		return memory.AuthorityHuman
	}
	return memory.AuthorityAgent
}

// IsHuman reports a person's observation.
func (o Observation) IsHuman() bool { return o.Authority() == memory.AuthorityHuman }

// Proof is how many facts support the observation.
func (o Observation) Proof() int { return len(o.Sources) }

// ObservationsPath is the bank's observations file.
func ObservationsPath(id string) string { return Prefix(id) + "observations.md" }

const obsTrailer = "<!--o "

// Format renders a current observation or a history entry as one bullet.
func (o Observation) Format() string {
	text := normFactText(o.Text)
	var b strings.Builder
	b.WriteString("- ")
	if o.Of != "" {
		b.WriteString("~~" + text + "~~")
	} else {
		b.WriteString(text)
	}
	b.WriteString(" " + obsTrailer)
	if o.Of != "" {
		b.WriteString("of=" + escapeField(o.Of))
		if !o.At.IsZero() {
			b.WriteString(" at=" + fmtTime(o.At))
		}
		if o.Deleted {
			b.WriteString(" deleted")
		}
	} else {
		b.WriteString("id=" + escapeField(o.ID))
		if o.Human || o.HandEdited {
			b.WriteString(" by=human")
		}
		b.WriteString(" sum=" + textSum(text))
	}
	if len(o.Sources) > 0 {
		b.WriteString(" src=" + joinComma(o.Sources))
		b.WriteString(" proof=" + itoa(len(o.Sources)))
	}
	if len(o.Evidence) > 0 {
		parts := make([]string, 0, len(o.Evidence))
		for _, ev := range o.Evidence {
			p := escapeField(ev.FactID)
			if q := strings.TrimSpace(ev.Quote); q != "" {
				p += ":" + escapeField(runeCut(oneLine(q), 160))
			}
			parts = append(parts, p)
		}
		b.WriteString(" ev=" + strings.Join(parts, ";"))
	}
	if len(o.Tags) > 0 {
		b.WriteString(" tags=" + joinComma(o.Tags))
	}
	if !o.OccStart.IsZero() {
		end := o.OccEnd
		if end.IsZero() {
			end = o.OccStart
		}
		b.WriteString(" occ=" + fmtTime(o.OccStart) + ".." + fmtTime(end))
	}
	if !o.Mentioned.IsZero() {
		b.WriteString(" men=" + fmtTime(o.Mentioned))
	}
	if !o.Updated.IsZero() && o.Of == "" {
		b.WriteString(" upd=" + fmtTime(o.Updated))
	}
	if o.Challenges != "" {
		b.WriteString(" chal=" + escapeField(o.Challenges))
	}
	b.WriteString(" -->")
	return b.String()
}

func runeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func stripStrike(s string) (string, bool) {
	t := strings.TrimSpace(s)
	if strings.HasPrefix(t, "~~") && strings.HasSuffix(t, "~~") && len(t) >= 4 {
		return strings.TrimSpace(t[2 : len(t)-2]), true
	}
	return t, false
}

// ParseObservationLine reads one bullet; ok is false for a non-bullet.
func ParseObservationLine(line, bankID string) (Observation, bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "- ") && !strings.HasPrefix(s, "* ") {
		return Observation{}, false
	}
	s = strings.TrimSpace(s[2:])
	var o Observation
	i := strings.LastIndex(s, obsTrailer)
	if i < 0 || !strings.HasSuffix(s, "-->") {
		text, _ := stripStrike(s)
		o.Text = normFactText(text)
		if o.Text == "" {
			return Observation{}, false
		}
		o.HandEdited = true
		o.ID = handObsID(bankID, o.Text)
		return o, true
	}
	text, _ := stripStrike(s[:i])
	o.Text = normFactText(text)
	if o.Text == "" {
		return Observation{}, false
	}
	for k, v := range trailerFields(strings.TrimSuffix(s[i+len(obsTrailer):], "-->")) {
		switch k {
		case "id":
			o.ID = unescapeField(v)
		case "sum":
			o.sum = v
		case "by":
			o.Human = v == "human"
		case "src":
			o.Sources = splitComma(v)
		case "ev":
			for _, part := range splitRaw(v, ";") {
				id, quote, _ := strings.Cut(part, ":")
				if id = unescapeField(id); id != "" {
					o.Evidence = append(o.Evidence, Evidence{FactID: id, Quote: unescapeField(quote)})
				}
			}
		case "tags":
			o.Tags = splitComma(v)
		case "occ":
			a, b, _ := strings.Cut(v, "..")
			o.OccStart, o.OccEnd = parseTime(a), parseTime(b)
			if o.OccEnd.IsZero() {
				o.OccEnd = o.OccStart
			}
		case "men":
			o.Mentioned = parseTime(v)
		case "upd":
			o.Updated = parseTime(v)
		case "chal":
			o.Challenges = unescapeField(v)
		case "of":
			o.Of = unescapeField(v)
		case "at":
			o.At = parseTime(v)
		case "deleted":
			o.Deleted = true
		}
	}
	if o.Of != "" {
		return o, true // history entries carry no id or authorship of their own
	}
	if o.ID == "" {
		o.ID = handObsID(bankID, o.Text)
		o.HandEdited = true
	}
	if o.sum != textSum(o.Text) {
		o.HandEdited = true
	}
	return o, true
}

// splitRaw splits without unescaping, for fields whose parts are themselves
// structured (ev's id:quote pairs).
func splitRaw(s, sep string) []string {
	if s == "" {
		return nil
	}
	var out []string
	for _, p := range strings.Split(s, sep) {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
}

func handObsID(bankID, text string) string {
	return "h" + shortHash(bankID+"\x00observation\x00"+normFactText(text), 15)
}

// ObservationsFile is the parsed observations file.
type ObservationsFile struct {
	Current []Observation
	History []Observation
	// Prose is every non-bullet line a person added, kept verbatim.
	Prose []string
}

const secHistory = "History"

// ParseObservations reads an observations file body.
func ParseObservations(body, bankID string) ObservationsFile {
	var out ObservationsFile
	inHistory := false
	seen := map[string]int{}
	for n, ln := range strings.Split(body, "\n") {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "## ") {
			inHistory = strings.EqualFold(strings.TrimSpace(t[3:]), secHistory)
			if !inHistory {
				out.Prose = append(out.Prose, ln)
			}
			continue
		}
		if o, ok := ParseObservationLine(ln, bankID); ok {
			o.Line = n
			if inHistory || o.Of != "" {
				if o.Of == "" {
					// A person wrote a plain bullet under History: keep it
					// as history of nothing rather than promoting it.
					o.Of = o.ID
				}
				out.History = append(out.History, o)
				continue
			}
			if c := seen[o.ID]; c > 0 {
				o.ID = o.ID + "-" + itoa(c)
			}
			seen[o.ID]++
			out.Current = append(out.Current, o)
			continue
		}
		if t == "" || strings.HasPrefix(t, "# Observations") || inHistory {
			continue
		}
		out.Prose = append(out.Prose, ln)
	}
	return out
}

// FormatObservations renders an observations file body.
func FormatObservations(of ObservationsFile) string {
	var b strings.Builder
	b.WriteString("# Observations\n\n")
	for _, o := range of.Current {
		b.WriteString(o.Format())
		b.WriteByte('\n')
	}
	if len(of.Prose) > 0 {
		b.WriteByte('\n')
		for _, p := range of.Prose {
			b.WriteString(p)
			b.WriteByte('\n')
		}
	}
	if len(of.History) > 0 {
		b.WriteString("\n## " + secHistory + "\n\n")
		for _, o := range of.History {
			b.WriteString(o.Format())
			b.WriteByte('\n')
		}
	}
	return b.String()
}

func observationsFrontmatter(bankID string) *markdown.Frontmatter {
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Observations: "+bankID)
	fm.Set("bank", bankID)
	return fm
}

// readObservations loads the observations file; a missing file is empty.
func (e *Engine) readObservations(bankID string) (ObservationsFile, string, error) {
	n, err := e.Vault.Read(ObservationsPath(bankID))
	if err != nil {
		if errors.Is(err, vault.ErrVault) {
			return ObservationsFile{}, "", nil
		}
		return ObservationsFile{}, "", err
	}
	return ParseObservations(n.Body, bankID), n.Body, nil
}

// writeObservations writes and indexes the observations file, keeping the
// previous version in history. Unchanged content writes nothing.
func (e *Engine) writeObservations(bankID string, of ObservationsFile, oldBody string) error {
	rel := ObservationsPath(bankID)
	body := FormatObservations(of)
	if strings.TrimSpace(body) == strings.TrimSpace(oldBody) {
		return nil
	}
	if e.History != nil && oldBody != "" {
		e.History.Snapshot(rel, oldBody)
	}
	if _, err := e.Vault.Write(rel, body, e.keepFrontmatter(rel, observationsFrontmatter(bankID))); err != nil {
		return err
	}
	_, err := e.Index.Upsert(rel)
	return err
}

// ObservationOut is one observation in a listing.
type ObservationOut struct {
	ID            string     `json:"id"`
	Text          string     `json:"text"`
	Authority     string     `json:"authority"`
	SourceFactIDs []string   `json:"source_fact_ids"`
	ProofCount    int        `json:"proof_count"`
	Evidence      []Evidence `json:"evidence,omitempty"`
	Tags          []string   `json:"tags"`
	OccurredStart string     `json:"occurred_start,omitempty"`
	OccurredEnd   string     `json:"occurred_end,omitempty"`
	MentionedAt   string     `json:"mentioned_at,omitempty"`
	UpdatedAt     string     `json:"updated_at,omitempty"`
	// Challenges is set on a model observation that disputes a person's.
	Challenges string `json:"challenges,omitempty"`
	// History entries carry Of (the observation they were a version of),
	// SupersededAt, and Deleted for a retirement.
	Of           string `json:"of,omitempty"`
	SupersededAt string `json:"superseded_at,omitempty"`
	Deleted      bool   `json:"deleted,omitempty"`
}

func (o Observation) out() ObservationOut {
	a := "agent"
	if o.IsHuman() {
		a = "human"
	}
	r := ObservationOut{ID: o.ID, Text: o.Text, Authority: a, SourceFactIDs: o.Sources, ProofCount: len(o.Sources),
		Evidence: o.Evidence, Tags: o.Tags, Challenges: o.Challenges, Of: o.Of, Deleted: o.Deleted}
	if r.SourceFactIDs == nil {
		r.SourceFactIDs = []string{}
	}
	if r.Tags == nil {
		r.Tags = []string{}
	}
	if o.Of != "" {
		r.ID, r.Authority = "", ""
	}
	if !o.OccStart.IsZero() {
		r.OccurredStart = o.OccStart.Format(time.RFC3339)
		r.OccurredEnd = o.OccEnd.Format(time.RFC3339)
	}
	if !o.Mentioned.IsZero() {
		r.MentionedAt = o.Mentioned.Format(time.RFC3339)
	}
	if !o.Updated.IsZero() {
		r.UpdatedAt = o.Updated.Format(time.RFC3339)
	}
	if !o.At.IsZero() {
		r.SupersededAt = o.At.Format(time.RFC3339)
	}
	return r
}

// ObservationQuery filters an observation listing.
type ObservationQuery struct {
	Query          string
	Human          *bool
	Tags           []string
	TagsMatch      string
	IncludeHistory bool
	Limit, Offset  int
}

// ListObservations lists a bank's current observations from the file, and
// with IncludeHistory the struck-through versions too.
func (e *Engine) ListObservations(bankID string, q ObservationQuery) (items, history []ObservationOut, total int, err error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, nil, 0, err
	}
	of, _, err := e.readObservations(bankID)
	if err != nil {
		return nil, nil, 0, err
	}
	needle := strings.ToLower(strings.TrimSpace(q.Query))
	match := q.TagsMatch
	if match == "" {
		match = "any"
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}
	items = []ObservationOut{}
	for _, o := range of.Current {
		if needle != "" && !strings.Contains(strings.ToLower(o.Text), needle) {
			continue
		}
		if q.Human != nil && o.IsHuman() != *q.Human {
			continue
		}
		if !tagsAllow(o.Tags, q.Tags, match) {
			continue
		}
		total++
		if total <= q.Offset || len(items) >= limit {
			continue
		}
		items = append(items, o.out())
	}
	if q.IncludeHistory {
		history = []ObservationOut{}
		for _, o := range of.History {
			history = append(history, o.out())
		}
	}
	return items, history, total, nil
}

// GetObservation returns one current observation with its history.
func (e *Engine) GetObservation(bankID, id string) (*ObservationOut, []ObservationOut, error) {
	if _, err := e.Profile(bankID); err != nil {
		return nil, nil, err
	}
	of, _, err := e.readObservations(bankID)
	if err != nil {
		return nil, nil, err
	}
	for _, o := range of.Current {
		if o.ID == id {
			out := o.out()
			hist := []ObservationOut{}
			for _, h := range of.History {
				if h.Of == id {
					hist = append(hist, h.out())
				}
			}
			sort.SliceStable(hist, func(a, b int) bool { return hist[a].SupersededAt > hist[b].SupersededAt })
			return &out, hist, nil
		}
	}
	return nil, nil, ErrNotFound
}

// DeleteObservation retires an observation by an explicit call. A person's
// observation needs force, as a person's fact does.
func (e *Engine) DeleteObservation(bankID, id string, force bool) error {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	of, old, err := e.readObservations(bankID)
	if err != nil {
		return err
	}
	for i, o := range of.Current {
		if o.ID != id {
			continue
		}
		if o.IsHuman() && !force {
			return ErrHumanProtected
		}
		of.Current = append(of.Current[:i:i], of.Current[i+1:]...)
		h := o
		h.Of, h.At, h.Deleted = o.ID, e.now(), true
		of.History = append(of.History, h)
		return e.writeObservations(bankID, of, old)
	}
	return ErrNotFound
}

// ClearObservations retires every model observation (moving them to
// history) and forgets which facts were consolidated, so the next
// consolidation starts over. A person's observations stay.
func (e *Engine) ClearObservations(bankID string) (int, error) {
	lock := e.bankLock(bankID)
	lock.Lock()
	defer lock.Unlock()
	of, old, err := e.readObservations(bankID)
	if err != nil {
		return 0, err
	}
	var keep []Observation
	n := 0
	for _, o := range of.Current {
		if o.IsHuman() {
			keep = append(keep, o)
			continue
		}
		h := o
		h.Of, h.At, h.Deleted = o.ID, e.now(), true
		of.History = append(of.History, h)
		n++
	}
	of.Current = keep
	if err := e.writeObservations(bankID, of, old); err != nil {
		return 0, err
	}
	return n, e.Index.DB.Exec("DELETE FROM bank_consolidated WHERE bank=?", bankID)
}
