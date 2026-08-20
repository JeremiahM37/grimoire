// Package index reconciles vault files into the SQLite index.
//
// Port of server/index.py. Files are truth; the index is a cache. Reindex()
// rebuilds everything; Upsert/Remove handle single-note changes from the API or
// the watcher. Backlinks fall out of the links table.
package index

import (
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"strings"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/pyjson"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Embedder is the vector backend the index writes with.
type Embedder interface {
	Embed(texts []string) [][]float32
	Signature() string
	Dim() int
}

// CommonsSpace is where a note lives when no space covers its path — and
// where every note lives on a deployment with no accounts.
const CommonsSpace = "commons"

// Spaces maps a note path to the access boundary it belongs to. Left nil on a
// single-user deployment, where every note is in the commons.
//
// An interface rather than a dependency on the auth package: the index knows
// that rows carry a space and that ranking filters on it, and nothing about
// accounts, membership or roles.
type Spaces interface {
	SpaceOf(path string) string
}

// Index owns the reconciliation between a vault and its SQLite cache.
type Index struct {
	DB     *db.DB
	Vault  *vault.Vault
	Emb    Embedder
	Spaces Spaces

	// writeMu serializes whole logical writes. See db.DB for why a
	// per-statement lock is not enough.
	writeMu sync.RWMutex
	rev     int64
	revMu   sync.Mutex

	// the retrieval cache, invalidated by rev; see retrieval.go
	cacheFields
	// the wiki-link lookup maps, patched on write; see resolve.go
	resolverFields
}

func New(database *db.DB, v *vault.Vault, e Embedder) *Index {
	return &Index{DB: database, Vault: v, Emb: e}
}

// Rev is a monotonic index-mutation counter used to invalidate caches. A
// counter rather than a table checksum, because SQLite can reuse freed rowids,
// so an edit-in-place could otherwise look unchanged.
func (ix *Index) Rev() int64 {
	ix.revMu.Lock()
	defer ix.revMu.Unlock()
	return ix.rev
}

func (ix *Index) bumpRev() {
	ix.revMu.Lock()
	ix.rev++
	ix.revMu.Unlock()
}

// Reindex rebuilds the whole index from the vault and returns the note count.
// It holds the write lock for the entire sweep so single-note upserts cannot
// interleave with the delete-then-insert pass.
func (ix *Index) Reindex() (int, error) {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	// Patching per note would cost more than the one rebuild this replaces.
	defer ix.endBulk()
	ix.beginBulk()

	// Bump before the deletes, not only inside writeNoteRows. A rebuild that
	// ends up writing no notes — an emptied or moved vault — would otherwise
	// never touch the revision, leaving the retrieval cache holding a corpus
	// whose rows have all been deleted, and searches answering from it
	// indefinitely without any error.
	ix.bumpRev()

	for _, tbl := range []string{"notes", "links", "tags", "fts", "fts_map", "facts", "vectors"} {
		if err := ix.DB.Exec("DELETE FROM " + tbl); err != nil {
			return 0, err
		}
	}
	rels, err := ix.Vault.Walk()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, rel := range rels {
		note, err := ix.Vault.Read(rel)
		if err != nil {
			continue // an unreadable file must not abort the rebuild
		}
		if err := ix.writeNoteRows(note); err != nil {
			return n, err
		}
		n++
	}
	if err := ix.resolveAll(); err != nil {
		return n, err
	}
	return n, nil
}

// Upsert indexes a single note from its file.
func (ix *Index) Upsert(rel string) (*vault.Note, error) {
	note, err := ix.Vault.Read(rel)
	if err != nil {
		return nil, err
	}
	if vault.IsReserved(rel) {
		return note, nil
	}
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	if err := ix.writeNoteRows(note); err != nil {
		return nil, err
	}
	// A new or edited note can resolve others' dangling links — but only the
	// ones that name it. Re-resolving the whole vault here cost 563ms per
	// write at 200,000 notes; see resolve.go.
	fmJSON, err := frontmatterJSON(note.Frontmatter)
	if err != nil {
		return nil, err
	}
	ix.noteResolves(note.Path, note.Title, fmJSON)
	if err := ix.resolveFor(note.Path, note.Title, fmJSON); err != nil {
		return nil, err
	}
	return note, nil
}

// The full-text row for a path is addressed through fts_map rather than by
// path, because fts.path is UNINDEXED: "DELETE FROM fts WHERE path=?" scans
// the whole FTS index, which is once per note on every write and once per note
// on every rebuild. That single statement is what made both quadratic — 22.4s
// to insert 16k notes against 442ms without it. RETURNING gets the new rowid
// in the same statement, so nothing depends on last_insert_rowid() surviving
// whatever else shares the connection.

func (ix *Index) dropFTS(rel string) error {
	if err := ix.DB.Exec(
		"DELETE FROM fts WHERE rowid IN (SELECT rid FROM fts_map WHERE path=?)", rel); err != nil {
		return err
	}
	return ix.DB.Exec("DELETE FROM fts_map WHERE path=?", rel)
}

func (ix *Index) insertFTS(rel, title, body string) error {
	var rid int64
	if err := ix.DB.QueryRow(
		"INSERT INTO fts_map(path) VALUES(?) RETURNING rid", rel).Scan(&rid); err != nil {
		return err
	}
	return ix.DB.Exec("INSERT INTO fts(rowid,path,title,body) VALUES(?,?,?,?)",
		rid, rel, title, body)
}

// Remove drops a note from the index.
func (ix *Index) Remove(rel string) error {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	if err := ix.removeRows(rel); err != nil {
		return err
	}
	ix.bumpRev()
	ix.noteGone(rel)
	// Links that pointed here are dangling now; nothing else changed.
	return ix.unresolveLinksTo(rel)
}

// splitList reads a comma-separated frontmatter value.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// spaceOf is the access boundary a note belongs to: the commons unless a
// deployment has spaces configured.
func (ix *Index) spaceOf(rel string) string {
	if ix.Spaces == nil {
		return CommonsSpace
	}
	if s := ix.Spaces.SpaceOf(rel); s != "" {
		return s
	}
	return CommonsSpace
}

// removeRows deletes one note's rows. The caller holds the write lock and owns
// the revision bump and link resolution, so a batch — a sync that found ten
// deleted notes — resolves links once rather than once per note, which is the
// difference between linear and quadratic on a large vault.
func (ix *Index) removeRows(rel string) error {
	for _, stmt := range []string{
		"DELETE FROM notes WHERE path=?",
		"DELETE FROM links WHERE src=?",
		"DELETE FROM tags WHERE note=?",
		"DELETE FROM vectors WHERE note=?",
		"DELETE FROM facts WHERE note=?",
	} {
		if err := ix.DB.Exec(stmt, rel); err != nil {
			return err
		}
	}
	if err := ix.dropFTS(rel); err != nil {
		return err
	}
	ix.patchNote(rel)
	return nil
}

func (ix *Index) writeNoteRows(note *vault.Note) error {
	ix.bumpRev()
	rel := note.Path
	for _, stmt := range []string{
		"DELETE FROM notes WHERE path=?",
		"DELETE FROM links WHERE src=?",
		"DELETE FROM tags WHERE note=?",
		"DELETE FROM vectors WHERE note=?",
		"DELETE FROM facts WHERE note=?",
	} {
		if err := ix.DB.Exec(stmt, rel); err != nil {
			return err
		}
	}
	if err := ix.dropFTS(rel); err != nil {
		return err
	}
	if !note.Encrypted {
		if err := ix.embedNote(note); err != nil {
			return err
		}
	}
	fmJSON, err := frontmatterJSON(note.Frontmatter)
	if err != nil {
		return err
	}
	private := 0
	if note.Private {
		private = 1
	}
	space := ix.spaceOf(rel)
	// A reader list travels with the note, in its frontmatter, so it survives
	// a reindex and is visible to whoever opens the file — the same property
	// that makes a path prefix a better boundary than a database row.
	acl := EncodeACL(splitList(note.Frontmatter.StringVal("readers")))
	if err := ix.DB.Exec(
		"INSERT INTO notes(path,title,body,frontmatter_json,private,mtime,hash,created,updated,space,acl)"+
			" VALUES(?,?,?,?,?,?,?,?,?,?,?)",
		rel, note.Title, note.Body, fmJSON, private, note.MTime, note.Hash,
		note.Frontmatter.StringVal("created"), note.Frontmatter.StringVal("updated"), space, acl,
	); err != nil {
		return err
	}
	// only the title is indexed for encrypted notes — ciphertext is never searchable
	ftsBody := note.Body
	if note.Encrypted {
		ftsBody = ""
	}
	if err := ix.insertFTS(rel, note.Title, ftsBody); err != nil {
		return err
	}
	for _, l := range note.Links {
		if err := ix.DB.Exec(
			"INSERT INTO links(src,target,alias,resolved) VALUES(?,?,?,0)",
			rel, l.Target, l.Alias); err != nil {
			return err
		}
	}
	for _, t := range note.Tags {
		if err := ix.DB.Exec("INSERT INTO tags(note,tag) VALUES(?,?)", rel, t); err != nil {
			return err
		}
	}
	if !note.Encrypted { // never mine facts out of ciphertext
		for _, f := range ExtractFacts(note.Body) {
			if err := ix.DB.Exec(
				"INSERT INTO facts(note,key,value,private) VALUES(?,?,?,?)",
				rel, f.Key, f.Value, private); err != nil {
				return err
			}
		}
	}
	// The retrieval cache is patched with this note's rows rather than thrown
	// away. Discarding it makes the next query rebuild the whole corpus —
	// 2.2s on a 50,000-note vault — which under continuous writes is a cost
	// paid by everyone, forever. See internal/index/cachepatch.go.
	ix.patchNote(rel)
	return nil
}

// embedNote chunks and embeds a note. Private notes are stored with a flag so
// retrieval can exclude them by default and opt in per query.
func (ix *Index) embedNote(note *vault.Note) error {
	chunks := embed.ChunkText(note.Title + "\n\n" + note.Body)
	if len(chunks) == 0 {
		return nil
	}
	vecs := ix.Emb.Embed(chunks)
	private := 0
	if note.Private {
		private = 1
	}
	for i, c := range chunks {
		if err := ix.DB.Exec(
			"INSERT INTO vectors(note,chunk_idx,chunk,embedding,private,space,acl) VALUES(?,?,?,?,?,?,?)",
			note.Path, i, c, Pack(vecs[i]), private, ix.spaceOf(note.Path),
			EncodeACL(splitList(note.Frontmatter.StringVal("readers")))); err != nil {
			return err
		}
	}
	return nil
}

// resolveAll points every link at a note path, by title, vault-relative path,
// filename stem, or a frontmatter alias.
func (ix *Index) resolveAll() error {
	rows, err := ix.DB.Query("SELECT path, title, frontmatter_json FROM notes")
	if err != nil {
		return err
	}
	byTitle := map[string]string{}
	byPath := map[string]string{}
	byStem := map[string]string{}
	byAlias := map[string]string{}
	for rows.Next() {
		var path, title, fmJSON string
		if err := rows.Scan(&path, &title, &fmJSON); err != nil {
			rows.Close()
			return err
		}
		byTitle[strings.ToLower(title)] = path
		// folder-qualified links — [[Job Search/Strategy]] — are how notes in
		// subfolders get linked; without this only root-level notes resolve
		setDefault(byPath, strings.ToLower(path), path)
		setDefault(byPath, strings.ToLower(strings.TrimSuffix(path, ".md")), path)
		stem := path
		if i := strings.LastIndex(stem, "/"); i >= 0 {
			stem = stem[i+1:]
		}
		setDefault(byStem, strings.ToLower(strings.TrimSuffix(stem, ".md")), path)
		for _, a := range aliasesOf(fmJSON) {
			setDefault(byAlias, strings.ToLower(a), path)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()

	linkRows, err := ix.DB.Query("SELECT rowid, target FROM links")
	if err != nil {
		return err
	}
	type upd struct {
		rowid int64
		dst   sql.NullString
	}
	var updates []upd
	for linkRows.Next() {
		var rowid int64
		var target string
		if err := linkRows.Scan(&rowid, &target); err != nil {
			linkRows.Close()
			return err
		}
		key := strings.ToLower(target)
		dst := ""
		for _, m := range []map[string]string{byTitle, byPath, byStem, byAlias} {
			if v, ok := m[key]; ok && v != "" {
				dst = v
				break
			}
		}
		updates = append(updates, upd{rowid, sql.NullString{String: dst, Valid: dst != ""}})
	}
	if err := linkRows.Err(); err != nil {
		return err
	}
	linkRows.Close()

	for _, u := range updates {
		resolved := 0
		if u.dst.Valid {
			resolved = 1
		}
		if err := ix.DB.Exec("UPDATE links SET dst=?, resolved=? WHERE rowid=?",
			u.dst, resolved, u.rowid); err != nil {
			return err
		}
	}
	return nil
}

func setDefault(m map[string]string, k, v string) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// AliasMap returns {alias_lower: path} across all notes, for link resolution in
// the UI.
func (ix *Index) AliasMap() (map[string]string, error) {
	rows, err := ix.DB.Query("SELECT path, frontmatter_json FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, fmJSON string
		if err := rows.Scan(&path, &fmJSON); err != nil {
			return nil, err
		}
		for _, a := range aliasesOf(fmJSON) {
			setDefault(out, strings.ToLower(a), path)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// AliasesOf exposes the alias parser to the API, so a caller-filtered alias
// map does not have to reimplement it and drift.
func AliasesOf(fmJSON string) []string { return aliasesOf(fmJSON) }

func aliasesOf(fmJSON string) []string {
	if fmJSON == "" {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(fmJSON), &m); err != nil {
		return nil
	}
	switch a := m["aliases"].(type) {
	case string:
		return []string{a}
	case []any:
		out := make([]string, 0, len(a))
		for _, x := range a {
			out = append(out, asString(x))
		}
		return out
	}
	return nil
}

func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "True"
		}
		return "False"
	case float64:
		if t == math.Trunc(t) {
			return strings.TrimSuffix(strings.TrimRight(formatFloat(t), "0"), ".")
		}
		return formatFloat(t)
	}
	return ""
}

func formatFloat(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// frontmatterJSON stores frontmatter the way Python's json.dumps would.
//
// This column is queried with LIKE patterns ('%"pinned": true%'), so the
// separators are load-bearing: Go's default "," / ":" would silently match
// nothing. ensure_ascii matters too — a title with an emoji must store the same
// bytes in both implementations. Key order follows the note, not sorted order.
func frontmatterJSON(fm *markdown.Frontmatter) (string, error) {
	if fm == nil || fm.Len() == 0 {
		return "{}", nil
	}
	pairs := make([]pyjson.Pair, 0, fm.Len())
	for _, k := range fm.Keys() {
		v, _ := fm.Get(k)
		pairs = append(pairs, pyjson.Pair{Key: k, Value: toAny(v)})
	}
	return pyjson.Object(pairs), nil
}

// toAny unwraps markdown.Value lists so the encoder sees plain Go types.
func toAny(v markdown.Value) any {
	if list, ok := v.([]markdown.Value); ok {
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = toAny(item)
		}
		return out
	}
	return v
}

// ------------------------------------------------------------------- facts

// Fact is a structured `key:: value` field declared in a note body.
type Fact struct {
	Key   string
	Value string
}

// factRE is the Dataview-style inline field. `::` must directly follow the key
// (no space), or ordinary prose containing " :: " would read as a fact.
var factRE = regexp.MustCompile(`^\s*(?:[-*]\s+)?([A-Za-z][\p{L}\p{M}\p{N}_/-]{0,48})::\s+(\S.*?)\s*$`)

// ExtractFacts returns the structured facts in a body. A projection of the
// markdown — agents can look these up deterministically instead of hoping
// retrieval surfaces the right sentence. Fenced code is skipped so a snippet
// containing `foo:: bar` isn't mistaken for one.
func ExtractFacts(body string) []Fact {
	var out []Fact
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := factRE.FindStringSubmatch(line); m != nil {
			out = append(out, Fact{
				Key:   strings.ToLower(strings.TrimSpace(m[1])),
				Value: strings.TrimSpace(m[2]),
			})
		}
	}
	return out
}
