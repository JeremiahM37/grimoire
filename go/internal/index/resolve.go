package index

import (
	"database/sql"
	"strings"
	"sync"
)

// Resolving wiki-links without re-resolving the vault.
//
// A [[link]] is written as a title, a path, a filename stem or an alias, and
// resolution turns that into the note it points at. Doing it for the whole
// vault is straightforward, and that is what happened on every single-note
// write: read every note, build four lookup maps, read every link, and issue an
// UPDATE for every one of them.
//
// At 200,000 notes one write cost 563ms and rewrote roughly 200,000 link rows
// to the values they already held. Same class as the two scaling problems
// already fixed here — work proportional to the corpus for a change that
// touched one note.
//
// A write now costs what it should: the note's own links, plus the links that
// were unresolved and might now point at it. The lookup maps are cached and
// patched in place, exactly like the retrieval cache, and a bulk rebuild still
// resolves everything at the end because that is genuinely a whole-vault change.

// resolver maps every form a link can be written in to the note it means.
type resolver struct {
	byTitle map[string]string
	byPath  map[string]string
	byStem  map[string]string
	byAlias map[string]string
}

type resolverFields struct {
	resolveMu sync.Mutex
	resolver  *resolver
}

// keysFor is every string that should resolve to this note.
func keysFor(path, title, fmJSON string) (titleKey string, pathKeys, stemKeys, aliasKeys []string) {
	titleKey = strings.ToLower(title)
	pathKeys = []string{strings.ToLower(path), strings.ToLower(strings.TrimSuffix(path, ".md"))}
	stem := path
	if i := strings.LastIndex(stem, "/"); i >= 0 {
		stem = stem[i+1:]
	}
	stemKeys = []string{strings.ToLower(strings.TrimSuffix(stem, ".md"))}
	for _, a := range aliasesOf(fmJSON) {
		aliasKeys = append(aliasKeys, strings.ToLower(a))
	}
	return
}

// resolverNow returns the lookup maps, building them once.
func (ix *Index) resolverNow() (*resolver, error) {
	ix.resolveMu.Lock()
	defer ix.resolveMu.Unlock()
	if ix.resolver != nil {
		return ix.resolver, nil
	}
	r := &resolver{
		byTitle: map[string]string{}, byPath: map[string]string{},
		byStem: map[string]string{}, byAlias: map[string]string{},
	}
	rows, err := ix.DB.Query("SELECT path, title, frontmatter_json FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, title, fmJSON string
		if err := rows.Scan(&path, &title, &fmJSON); err != nil {
			return nil, err
		}
		r.add(path, title, fmJSON)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ix.resolver = r
	return r, nil
}

func (r *resolver) add(path, title, fmJSON string) {
	titleKey, pathKeys, stemKeys, aliasKeys := keysFor(path, title, fmJSON)
	r.byTitle[titleKey] = path
	for _, k := range pathKeys {
		setDefault(r.byPath, k, path)
	}
	for _, k := range stemKeys {
		setDefault(r.byStem, k, path)
	}
	for _, k := range aliasKeys {
		setDefault(r.byAlias, k, path)
	}
}

// forget removes a note's keys, so a deleted note stops resolving.
func (r *resolver) forget(path string) {
	for _, m := range []map[string]string{r.byTitle, r.byPath, r.byStem, r.byAlias} {
		for k, v := range m {
			if v == path {
				delete(m, k)
			}
		}
	}
}

// lookup finds the note a link target means.
func (r *resolver) lookup(target string) string {
	key := strings.ToLower(target)
	for _, m := range []map[string]string{r.byTitle, r.byPath, r.byStem, r.byAlias} {
		if v, ok := m[key]; ok && v != "" {
			return v
		}
	}
	return ""
}

// invalidateResolver drops the cached maps; a bulk operation rebuilds them.
func (ix *Index) invalidateResolver() {
	ix.resolveMu.Lock()
	ix.resolver = nil
	ix.resolveMu.Unlock()
}

// noteResolves records a note in the cached maps after it is written.
func (ix *Index) noteResolves(path, title, fmJSON string) {
	ix.resolveMu.Lock()
	if ix.resolver != nil {
		ix.resolver.forget(path) // a retitled note must stop answering to its old title
		ix.resolver.add(path, title, fmJSON)
	}
	ix.resolveMu.Unlock()
}

// noteGone removes a note from the cached maps.
func (ix *Index) noteGone(path string) {
	ix.resolveMu.Lock()
	if ix.resolver != nil {
		ix.resolver.forget(path)
	}
	ix.resolveMu.Unlock()
}

// resolveFor updates only the links one note's write can affect: the links it
// contains, and the links that were unresolved and might now point at it.
func (ix *Index) resolveFor(path, title, fmJSON string) error {
	r, err := ix.resolverNow()
	if err != nil {
		return err
	}

	// This note's own links.
	if err := ix.updateLinks("SELECT rowid, target, dst, resolved FROM links WHERE src=?",
		[]any{path}, func(target string) string { return r.lookup(target) }); err != nil {
		return err
	}

	// Links that could not be resolved before and might now be: only the ones
	// whose target is a name this note answers to.
	titleKey, pathKeys, stemKeys, aliasKeys := keysFor(path, title, fmJSON)
	claims := map[string]bool{titleKey: true}
	for _, group := range [][]string{pathKeys, stemKeys, aliasKeys} {
		for _, k := range group {
			claims[k] = true
		}
	}
	return ix.updateLinks("SELECT rowid, target, dst, resolved FROM links WHERE resolved=0",
		nil, func(target string) string {
			if claims[strings.ToLower(target)] {
				return path
			}
			return ""
		})
}

// unresolveLinksTo marks links pointing at a deleted note as dangling.
func (ix *Index) unresolveLinksTo(path string) error {
	return ix.DB.Exec("UPDATE links SET dst=NULL, resolved=0 WHERE dst=?", path)
}

// updateLinks applies a resolution function to a set of links, writing only the
// rows whose answer actually changed.
//
// Writing every row unconditionally is what made this expensive: in a settled
// vault almost every link already holds the value being written to it.
func (ix *Index) updateLinks(query string, args []any, resolve func(target string) string) error {
	rows, err := ix.DB.Query(query, args...)
	if err != nil {
		return err
	}
	type change struct {
		rowid int64
		dst   sql.NullString
	}
	var changes []change
	for rows.Next() {
		var rowid int64
		var target string
		var dst sql.NullString
		var resolved int
		if err := rows.Scan(&rowid, &target, &dst, &resolved); err != nil {
			rows.Close()
			return err
		}
		want := resolve(target)
		if want == "" && resolved == 0 && !dst.Valid {
			continue // already dangling
		}
		if want != "" && dst.Valid && dst.String == want && resolved == 1 {
			continue // already correct
		}
		if want == "" {
			// A link this pass has nothing to say about — an unresolved link
			// whose target is not this note — is left exactly as it was.
			continue
		}
		changes = append(changes, change{rowid, sql.NullString{String: want, Valid: true}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, c := range changes {
		if err := ix.DB.Exec("UPDATE links SET dst=?, resolved=1 WHERE rowid=?",
			c.dst, c.rowid); err != nil {
			return err
		}
	}
	return nil
}
