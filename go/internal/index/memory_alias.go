package index

import (
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Alias links in the index: the derived side of memory/alias.go.
//
// Three tables, all rebuilt from the markdown and none of them written back:
//
//   - memory_mentions: the full entity names each fact contains;
//   - memory_alias_src: the evidence, one row per (short form, entity, fact);
//   - memory_links: the resolved link per (fact, short form), with its scope.
//
// A write does not re-resolve the whole store. It marks the short forms it
// touched as dirty: the forms its own evidence added or removed, and the forms
// its facts use that the table already knows. The flush then re-resolves only
// the facts that contain a dirty form, and writes only the links that changed.
// During a full reindex the marks accumulate and one flush runs at the end, so
// a rebuild costs one pass over the facts, not one per note.
//
// Deleting a note removes its evidence and marks the forms it established, so
// links that depended on it are recomputed and disappear. Nothing is cached
// across a reindex: the tables are a function of the vault.

// aliasFields is the write-side state: the forms awaiting re-resolution. It is
// only touched under the write lock.
type aliasFields struct {
	aliasDirty map[string]bool
}

// markAliasDirty queues a short form for re-resolution.
func (ix *Index) markAliasDirty(k string) {
	if ix.aliasDirty == nil {
		ix.aliasDirty = map[string]bool{}
	}
	ix.aliasDirty[k] = true
}

// aliasStmt is one statement of an alias write.
type aliasStmt struct {
	query string
	args  []any
}

// execAliasBatch runs statements in one transaction under the database lock.
//
// Each autocommitted statement is its own fsync, and an alias write is several
// rows per fact. Batched per note, the same rows cost one commit. The caller
// must not hold the database lock or another transaction.
func (ix *Index) execAliasBatch(stmts []aliasStmt) error {
	if len(stmts) == 0 {
		return nil
	}
	ix.DB.Lock()
	defer ix.DB.Unlock()
	tx, err := ix.DB.Conn().Begin()
	if err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.Exec(s.query, s.args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// aliasRemoveRows is called before a note's rows are deleted. It queues the
// forms that note's evidence established, because facts elsewhere may have
// been linked through them, and removes the note's own alias rows.
func (ix *Index) aliasRemoveRows(rel string) error {
	rows, err := ix.DB.Query("SELECT DISTINCT alias FROM memory_alias_src WHERE note=?", rel)
	if err != nil {
		return err
	}
	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, k := range keys {
		ix.markAliasDirty(k)
	}
	return ix.execAliasBatch([]aliasStmt{
		{"DELETE FROM memory_mentions WHERE note=?", []any{rel}},
		{"DELETE FROM memory_alias_src WHERE note=?", []any{rel}},
		{"DELETE FROM memory_links WHERE note=?", []any{rel}},
	})
}

// aliasWriteRows records one note's mentions and evidence, and queues the
// forms they touch. Called after the note's memory_entries rows are written.
func (ix *Index) aliasWriteRows(rel string, entries []memory.Entry) error {
	var candidates []string
	var stmts []aliasStmt
	for _, e := range entries {
		for _, m := range memory.FullMentions(e.Text) {
			stmts = append(stmts, aliasStmt{
				"INSERT OR IGNORE INTO memory_mentions(note,id,canon) VALUES(?,?,?)",
				[]any{rel, e.ID, m.Canon}})
			for _, k := range m.Keys {
				stmts = append(stmts, aliasStmt{
					"INSERT OR IGNORE INTO memory_alias_src(alias,canon,note,id,session,stamp) VALUES(?,?,?,?,?,?)",
					[]any{k, m.Canon, rel, e.ID, e.Session, e.Stamp}})
				ix.markAliasDirty(k)
			}
		}
		candidates = append(candidates, memory.AliasCandidates(e.Text)...)
	}
	if err := ix.execAliasBatch(stmts); err != nil {
		return err
	}
	// Facts in this note may use a short form the table already knows. Those
	// are queued too, because the note's rows were just deleted and rewritten,
	// and nothing else would relink them.
	known, err := ix.knownAliases(candidates)
	if err != nil {
		return err
	}
	for k := range known {
		ix.markAliasDirty(k)
	}
	return nil
}

// knownAliases returns which of keys have any evidence.
func (ix *Index) knownAliases(keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	uniq := dedupe(keys)
	for start := 0; start < len(uniq); start += fetchChunk {
		end := min(start+fetchChunk, len(uniq))
		chunk := uniq[start:end]
		rows, err := ix.DB.Query("SELECT DISTINCT alias FROM memory_alias_src WHERE alias IN ("+
			placeholders(len(chunk))+")", anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k string
			if err := rows.Scan(&k); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// fullScanKeys is the dirty-set size above which the flush reads every fact
// once instead of running one FTS query per form. A rebuild is the case that
// goes over it; an ordinary write is a handful of forms.
const fullScanKeys = 200

// flushAliases re-resolves every fact that contains a dirty form and writes
// the difference. It is a no-op when nothing is dirty. The caller holds the
// write lock.
func (ix *Index) flushAliases() error {
	if len(ix.aliasDirty) == 0 {
		return nil
	}
	dirty := make([]string, 0, len(ix.aliasDirty))
	for k := range ix.aliasDirty {
		dirty = append(dirty, k)
	}
	sort.Strings(dirty)
	ix.aliasDirty = nil
	dirtySet := make(map[string]bool, len(dirty))
	for _, k := range dirty {
		dirtySet[k] = true
	}

	ev, err := ix.loadAliasEvidence(dirty)
	if err != nil {
		return err
	}
	facts, err := ix.aliasFacts(dirty, dirtySet)
	if err != nil {
		return err
	}
	old, err := ix.loadAliasLinks(dirty)
	if err != nil {
		return err
	}

	wanted := map[aliasLinkKey]aliasLinkRow{}
	resolvers := map[string]*memory.AliasResolver{}
	for _, f := range facts {
		stamp, _ := memory.StampTime(f.stamp)
		ref := memory.FactRef{Note: f.note, ID: f.id, Session: f.session, Stamp: stamp}
		self := map[string]bool{}
		for _, m := range f.mentions {
			ref.Canons = append(ref.Canons, m.Canon)
			for _, k := range m.Keys {
				self[k] = true
			}
		}
		for _, k := range f.keys {
			if !dirtySet[k] || self[k] || len(ev[k]) == 0 {
				continue
			}
			r := resolvers[k]
			if r == nil {
				r = memory.NewAliasResolver(k, ev[k])
				resolvers[k] = r
			}
			link, outcome := r.Resolve(ref)
			if outcome != memory.Linked {
				continue
			}
			wanted[aliasLinkKey{f.note, f.id, k}] = aliasLinkRow{
				entity: link.Entity, scope: link.Scope, conf: link.Conf}
		}
	}

	// Write only the difference. A settled vault has almost every link already
	// in place, and rewriting them all is what made the link table costly.
	var changes []aliasStmt
	for lk, row := range wanted {
		if prev, ok := old[lk]; ok && prev == row {
			continue
		}
		changes = append(changes, aliasStmt{
			"INSERT OR REPLACE INTO memory_links(note,id,alias,entity,scope,conf) VALUES(?,?,?,?,?,?)",
			[]any{lk.note, lk.id, lk.alias, row.entity, row.scope, row.conf}})
	}
	for lk := range old {
		if _, ok := wanted[lk]; ok {
			continue
		}
		changes = append(changes, aliasStmt{
			"DELETE FROM memory_links WHERE note=? AND id=? AND alias=?",
			[]any{lk.note, lk.id, lk.alias}})
	}
	return ix.execAliasBatch(changes)
}

// flushAliasesUnlessBulk runs the flush outside a bulk rebuild, which runs its
// own at the end.
func (ix *Index) flushAliasesUnlessBulk() error {
	if ix.bulk {
		return nil
	}
	return ix.flushAliases()
}

type aliasLinkKey struct{ note, id, alias string }

type aliasLinkRow struct {
	entity, scope string
	conf          float64
}

// loadAliasEvidence reads every evidence row for the forms, with the full
// names of each fact it sits in, grouped by form.
func (ix *Index) loadAliasEvidence(keys []string) (map[string][]memory.Evidence, error) {
	type evKey struct{ alias, canon, note, id string }
	byKey := map[evKey]*memory.Evidence{}
	var order []evKey
	for start := 0; start < len(keys); start += fetchChunk {
		end := min(start+fetchChunk, len(keys))
		chunk := keys[start:end]
		rows, err := ix.DB.Query(
			"SELECT a.alias, a.canon, a.note, a.id, a.session, a.stamp, m.canon"+
				" FROM memory_alias_src a JOIN memory_mentions m ON m.note=a.note AND m.id=a.id"+
				" WHERE a.alias IN ("+placeholders(len(chunk))+")", anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var alias, canon, note, id, session, stamp, other string
			if err := rows.Scan(&alias, &canon, &note, &id, &session, &stamp, &other); err != nil {
				rows.Close()
				return nil, err
			}
			k := evKey{alias, canon, note, id}
			e := byKey[k]
			if e == nil {
				t, _ := memory.StampTime(stamp)
				e = &memory.Evidence{Alias: alias, Canon: canon, Note: note, ID: id,
					Session: session, Stamp: t}
				byKey[k] = e
				order = append(order, k)
			}
			e.Canons = append(e.Canons, other)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	out := map[string][]memory.Evidence{}
	for _, k := range order {
		e := byKey[k]
		out[k.alias] = append(out[k.alias], *e)
	}
	return out, nil
}

// aliasFact is a fact that may use a dirty form, with the extraction the
// flush needs. It is computed once here: the regexes are the cost of a flush,
// and a fact is visited from several forms.
type aliasFact struct {
	note, id, session, stamp, text string
	keys                           []string // AliasCandidates(text), capped
	mentions                       []memory.Mention
}

// aliasFacts returns the facts that contain at least one dirty form. A
// rebuild reads every fact once; an ordinary write asks FTS for each form.
// FTS is a superset (stemming), so each candidate is checked against the
// exact keys before it is used.
func (ix *Index) aliasFacts(dirty []string, dirtySet map[string]bool) ([]aliasFact, error) {
	seen := map[string]bool{}
	var out []aliasFact
	add := func(f aliasFact) {
		k := f.note + "\x00" + f.id
		if seen[k] {
			return
		}
		f.keys = memory.AliasCandidates(f.text)
		for _, c := range f.keys {
			if dirtySet[c] {
				seen[k] = true
				f.mentions = memory.FullMentions(f.text)
				out = append(out, f)
				return
			}
		}
	}
	if len(dirty) > fullScanKeys {
		rows, err := ix.DB.Query("SELECT note,id,session,stamp,text FROM memory_entries")
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var f aliasFact
			if err := rows.Scan(&f.note, &f.id, &f.session, &f.stamp, &f.text); err != nil {
				return nil, err
			}
			add(f)
		}
		return out, rows.Err()
	}
	for _, k := range dirty {
		rows, err := ix.DB.Query(
			"SELECT e.note,e.id,e.session,e.stamp,e.text FROM memory_fts"+
				" JOIN memory_entries e ON e.rowid = memory_fts.rowid"+
				" WHERE memory_fts MATCH ?", `"`+strings.ReplaceAll(k, `"`, `""`)+`"`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f aliasFact
			if err := rows.Scan(&f.note, &f.id, &f.session, &f.stamp, &f.text); err != nil {
				rows.Close()
				return nil, err
			}
			add(f)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// loadAliasLinks reads the links currently stored for the forms.
func (ix *Index) loadAliasLinks(keys []string) (map[aliasLinkKey]aliasLinkRow, error) {
	out := map[aliasLinkKey]aliasLinkRow{}
	for start := 0; start < len(keys); start += fetchChunk {
		end := min(start+fetchChunk, len(keys))
		chunk := keys[start:end]
		rows, err := ix.DB.Query(
			"SELECT note,id,alias,entity,scope,conf FROM memory_links WHERE alias IN ("+
				placeholders(len(chunk))+")", anys(chunk)...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var k aliasLinkKey
			var r aliasLinkRow
			if err := rows.Scan(&k.note, &k.id, &k.alias, &r.entity, &r.scope, &r.conf); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// aliasLinkScore is the entity-score credit a query gets from a link.
type aliasLinkScore struct {
	score float64
	scope string
}

// candidateLinks returns, for each candidate, the best link to an entity the
// query names in full. A fact linked to nothing the query names is absent.
func (ix *Index) candidateLinks(cands []memoryRow, canons []string) map[entKey]aliasLinkScore {
	out := map[entKey]aliasLinkScore{}
	if len(cands) == 0 || len(canons) == 0 {
		return out
	}
	want := make(map[entKey]bool, len(cands))
	for _, c := range cands {
		want[entKey{c.hit.Note, c.hit.ID}] = true
	}
	uniq := dedupe(canons)
	rows, err := ix.DB.Query("SELECT note,id,entity,scope,conf FROM memory_links WHERE entity IN ("+
		placeholders(len(uniq))+")", anys(uniq)...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var k entKey
		var scope string
		var conf float64
		var entity string
		if err := rows.Scan(&k.note, &k.id, &entity, &scope, &conf); err != nil {
			break
		}
		if !want[k] {
			continue
		}
		s := memory.LinkScore(conf)
		if prev, ok := out[k]; !ok || s > prev.score {
			out[k] = aliasLinkScore{score: s, scope: scope}
		}
	}
	return out
}

// dedupe returns the distinct strings, in first-seen order.
func dedupe(xs []string) []string {
	seen := make(map[string]bool, len(xs))
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}

func anys(xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = x
	}
	return out
}

// aliasTablesKey records that the alias tables were built for this index, and
// under which rules. Bump aliasTablesVersion when extraction or resolution
// changes in a way that makes stored links wrong; the next sync rebuilds them.
const (
	aliasTablesKey     = "alias_tables"
	aliasTablesVersion = "1"
)

// aliasBackfill builds the alias tables from the memory rows the index already
// holds, once, when they have never been built or were built under older
// rules. It reads no note and embeds nothing: the tables are a function of
// each fact's text, session and stamp, all of which are in memory_entries. The
// caller holds the write lock.
func (ix *Index) aliasBackfill() error {
	have, err := ix.meta(aliasTablesKey)
	if err != nil {
		return err
	}
	if have == aliasTablesVersion {
		return nil
	}
	for _, tbl := range []string{"memory_mentions", "memory_alias_src", "memory_links"} {
		if err := ix.DB.Exec("DELETE FROM " + tbl); err != nil {
			return err
		}
	}
	rows, err := ix.DB.Query("SELECT note,id,text,session,stamp FROM memory_entries ORDER BY note, line")
	if err != nil {
		return err
	}
	type noteFacts struct {
		rel     string
		entries []memory.Entry
	}
	var notes []noteFacts
	for rows.Next() {
		var rel string
		var e memory.Entry
		if err := rows.Scan(&rel, &e.ID, &e.Text, &e.Session, &e.Stamp); err != nil {
			rows.Close()
			return err
		}
		if n := len(notes); n == 0 || notes[n-1].rel != rel {
			notes = append(notes, noteFacts{rel: rel})
		}
		notes[len(notes)-1].entries = append(notes[len(notes)-1].entries, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	// The database is one connection: the read is closed before any write.
	rows.Close()
	for _, n := range notes {
		if err := ix.aliasWriteRows(n.rel, n.entries); err != nil {
			return err
		}
	}
	if err := ix.flushAliases(); err != nil {
		return err
	}
	return ix.setMeta(aliasTablesKey, aliasTablesVersion)
}
