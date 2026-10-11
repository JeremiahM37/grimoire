package index

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Contextual headers for memory facts (experiment flag "header").
//
// A fact is one sentence, so its embedding and its keyword match see only
// that sentence. The header adds what the index already knows about the fact
// and not what the sentence says: the note's title, the full names of the
// entities it is about (its own full-name mentions plus the names the alias
// linker resolves its short forms to), and the date. The embedder is given
// "header. text"; the keyword arm matches query terms against "header text".
// The stored fact text, and everything recall returns, is unchanged.
//
// No model is involved. The header is a function of the note title, the
// fact's stamp, memory_mentions and memory_links, so it can be recomputed
// from the index alone. It is refreshed whenever the alias linker changes a
// fact's links, because a header that names an entity the fact no longer
// links to would embed a stale claim.
//
// Which fields are used is chosen by GRIMOIRE_HEADER_FIELDS (a comma list of
// title, entities, date). The default is all three. Off, nothing here runs and
// the index is the one it always was.
//
// Not covered: memory_fts, the FTS5 table the over-bound lexical arm reads,
// is an external-content table over memory_entries.text. Putting the header
// there needs a schema change, so the lexical candidate arm sees the text only.
// The window path (the measured sizes below the scan bound) does not use FTS.

const headerFlag = "header"

var (
	headerFieldsOnce sync.Once
	headerFieldsSet  map[string]bool
)

// headerOn reports whether the header experiment is enabled.
func headerOn() bool { return exp.On(headerFlag) }

// headerFields is the set of header fields in effect for this process.
func headerFields() map[string]bool {
	headerFieldsOnce.Do(func() {
		raw := os.Getenv("GRIMOIRE_HEADER_FIELDS")
		if strings.TrimSpace(raw) == "" {
			raw = "title,entities,date"
		}
		headerFieldsSet = map[string]bool{}
		for _, f := range strings.Split(raw, ",") {
			if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
				headerFieldsSet[f] = true
			}
		}
	})
	return headerFieldsSet
}

// renderHeader joins the chosen fields with ". ". An empty result means the
// fact is embedded as its bare text, exactly as with the experiment off.
func renderHeader(fields map[string]bool, title string, entities []string, date string) string {
	var parts []string
	if fields["title"] && title != "" {
		parts = append(parts, title)
	}
	if fields["entities"] && len(entities) > 0 {
		parts = append(parts, strings.Join(entities, ", "))
	}
	if fields["date"] && date != "" {
		parts = append(parts, date)
	}
	return strings.Join(parts, ". ")
}

// withHeader is the text the embedder and the keyword arm see for a fact.
func withHeader(header, text string) string {
	if header == "" {
		return text
	}
	return header + ". " + text
}

// headerDate is the calendar day of a stamp ("2006-01-02 15:04" or a date).
func headerDate(stamp string) string {
	if len(stamp) >= 10 {
		return stamp[:10]
	}
	return ""
}

// ensureHeaderTable creates the side table on first use under the flag. It is
// not in the base schema, so an index built with the flag off is unchanged.
func (ix *Index) ensureHeaderTable() error {
	return ix.DB.Exec(
		"CREATE TABLE IF NOT EXISTS memory_headers(" +
			"note TEXT NOT NULL, id TEXT NOT NULL, title TEXT NOT NULL, date TEXT NOT NULL, " +
			"header TEXT NOT NULL, PRIMARY KEY(note, id))")
}

// writeHeaders computes the header of every entry of a note being written and
// stores it. It returns the header per entry, index-aligned with entries, or
// nil when the experiment is off. Links are read as they stand now: the alias
// flush that follows a write re-resolves links and calls refreshHeaders for
// any fact whose set changed, so the stored header is always the one for the
// links the index currently holds.
func (ix *Index) writeHeaders(note *vault.Note, entries []memory.Entry) ([]string, error) {
	if !headerOn() || len(entries) == 0 {
		return nil, nil
	}
	if err := ix.ensureHeaderTable(); err != nil {
		return nil, err
	}
	linked, err := ix.linkedEntities(note.Path)
	if err != nil {
		return nil, err
	}
	fields := headerFields()
	out := make([]string, len(entries))
	for i, e := range entries {
		var canons []string
		for _, m := range memory.FullMentions(e.Text) {
			canons = append(canons, m.Canon)
		}
		ents := mergeNames(canons, linked[e.ID])
		out[i] = renderHeader(fields, note.Title, ents, headerDate(e.Stamp))
		if err := ix.DB.Exec(
			"INSERT OR REPLACE INTO memory_headers(note,id,title,date,header) VALUES(?,?,?,?,?)",
			note.Path, e.ID, note.Title, headerDate(e.Stamp), out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// linkedEntities returns, per fact id in a note, the entities its short forms
// are linked to.
func (ix *Index) linkedEntities(rel string) (map[string][]string, error) {
	out := map[string][]string{}
	rows, err := ix.DB.Query("SELECT id, entity FROM memory_links WHERE note=?", rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, ent string
		if err := rows.Scan(&id, &ent); err != nil {
			return nil, err
		}
		out[id] = append(out[id], ent)
	}
	return out, rows.Err()
}

// mergeNames is the sorted, de-duplicated union of two name lists.
func mergeNames(a, b []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, xs := range [][]string{a, b} {
		for _, x := range xs {
			if x != "" && !seen[x] {
				seen[x] = true
				out = append(out, x)
			}
		}
	}
	sort.Strings(out)
	return out
}

// dropHeaderRows removes a note's header rows before its facts are rewritten.
func (ix *Index) dropHeaderRows(rel string) error {
	if !headerOn() {
		return nil
	}
	if err := ix.ensureHeaderTable(); err != nil {
		return err
	}
	return ix.DB.Exec("DELETE FROM memory_headers WHERE note=?", rel)
}

// refreshHeadersAfterLinks runs after the alias flush has written link
// changes. Every fact a change touched gets its header rebuilt from the index
// and is embedded again, in one batch.
func (ix *Index) refreshHeadersAfterLinks(changes []aliasStmt) error {
	if !headerOn() || len(changes) == 0 {
		return nil
	}
	seen := map[headerRef]bool{}
	var refs []headerRef
	for _, c := range changes {
		if len(c.args) < 2 {
			continue
		}
		r := headerRef{fmt.Sprint(c.args[0]), fmt.Sprint(c.args[1])}
		if !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	return ix.refreshHeaders(refs)
}

// refreshHeaders rebuilds the header and the embedding of the given facts.
// Facts with no header row (written before the flag was on) are skipped.
func (ix *Index) refreshHeaders(refs []headerRef) error {
	if len(refs) == 0 {
		return nil
	}
	fields := headerFields()
	type item struct {
		headerRef
		header, text string
	}
	var items []item
	for _, r := range refs {
		var title, date, text string
		err := ix.DB.QueryRow(
			"SELECT h.title, h.date, e.text FROM memory_headers h"+
				" JOIN memory_entries e ON e.note=h.note AND e.id=h.id"+
				" WHERE h.note=? AND h.id=?", r.note, r.id).Scan(&title, &date, &text)
		if err != nil {
			continue
		}
		canons, err := ix.mentionsOf(r.note, r.id)
		if err != nil {
			return err
		}
		links, err := ix.linksOf(r.note, r.id)
		if err != nil {
			return err
		}
		header := renderHeader(fields, title, mergeNames(canons, links), date)
		items = append(items, item{r, header, text})
	}
	if len(items) == 0 {
		return nil
	}
	texts := make([]string, len(items))
	for i, it := range items {
		texts[i] = withHeader(it.header, it.text)
	}
	vecs := ix.Emb.Embed(texts)
	for i, it := range items {
		if i >= len(vecs) {
			break
		}
		if err := ix.DB.Exec("UPDATE memory_headers SET header=? WHERE note=? AND id=?",
			it.header, it.note, it.id); err != nil {
			return err
		}
		if err := ix.DB.Exec("UPDATE memory_entries SET embedding=? WHERE note=? AND id=?",
			Pack(vecs[i]), it.note, it.id); err != nil {
			return err
		}
	}
	return nil
}

// headerRef names one fact.
type headerRef struct{ note, id string }

func (ix *Index) mentionsOf(note, id string) ([]string, error) {
	rows, err := ix.DB.Query("SELECT canon FROM memory_mentions WHERE note=? AND id=?", note, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (ix *Index) linksOf(note, id string) ([]string, error) {
	rows, err := ix.DB.Query("SELECT entity FROM memory_links WHERE note=? AND id=?", note, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// candidateHeaders returns the stored header of each candidate, keyed like
// candidateEntities. It is nil when the experiment is off, or when the header
// table does not exist yet (an index built with the flag off, queried with it
// on): the keyword arm then falls back to the text alone.
func (ix *Index) candidateHeaders(cands []memoryRow) map[entKey]string {
	if !headerOn() || len(cands) == 0 {
		return nil
	}
	out := make(map[entKey]string, len(cands))
	notes := map[string]bool{}
	for _, c := range cands {
		notes[c.hit.Note] = true
	}
	args := make([]any, 0, len(notes))
	marks := make([]string, 0, len(notes))
	for n := range notes {
		args = append(args, n)
		marks = append(marks, "?")
	}
	rows, err := ix.DB.Query("SELECT note,id,header FROM memory_headers WHERE note IN ("+
		strings.Join(marks, ",")+")", args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k entKey
		var h string
		if err := rows.Scan(&k.note, &k.id, &h); err != nil {
			break
		}
		out[k] = h
	}
	return out
}
