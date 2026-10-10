package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Facts, tag rename, graph, tasks and completion.
// Port of the remainder of server/routers/search.py and misc.py.

// facts serves the structured `key:: value` layer projected from note bodies —
// a deterministic lookup over the same markdown. Private notes' facts are
// excluded unless explicitly included.
func (s *Server) facts(w http.ResponseWriter, r *http.Request) {
	var conds []string
	var args []any
	if k := strings.TrimSpace(r.URL.Query().Get("key")); k != "" {
		conds = append(conds, "f.key=?")
		args = append(args, strings.ToLower(k))
	}
	if n := strings.TrimSpace(r.URL.Query().Get("note")); n != "" {
		conds = append(conds, "f.note=?")
		args = append(args, n)
	}
	if !truthy(r.URL.Query().Get("include_private")) {
		// Qualified: `private` exists in facts AND notes, and an unqualified
		// reference became ambiguous the moment the join was added.
		conds = append(conds, "f.private=0")
	}
	q := "SELECT f.note, f.key, f.value, COALESCE(n.acl,'') FROM facts f LEFT JOIN notes n ON n.path=f.note"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY f.key, f.note"

	rows, err := s.Index.DB.Query(q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var note, key, value, acl string
		if err := rows.Scan(&note, &key, &value, &acl); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !s.canReadNote(r, note, acl) {
			continue
		}
		out = append(out, map[string]string{"note": note, "key": key, "value": value})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func truthy(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

type tagRename struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// renameTag renames #old → #new across every note, in bodies and frontmatter.
// Encrypted notes only have their frontmatter tags changed — the ciphertext
// body is left untouched.
func (s *Server) renameTag(w http.ResponseWriter, r *http.Request) {
	var in tagRename
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	old := strings.TrimPrefix(strings.TrimSpace(in.Old), "#")
	nw := strings.TrimPrefix(strings.TrimSpace(in.New), "#")
	if old == "" || nw == "" {
		writeErr(w, http.StatusBadRequest, "old and new tag names required")
		return
	}
	// match '#old' as a whole tag, not '#oldsuffix' or 'word#old'. RE2 has no
	// lookaround, so the boundaries are captured and re-emitted.
	pat := regexp.MustCompile(`(^|[^\p{L}\p{N}_#/])#` + regexp.QuoteMeta(old) +
		`($|[^\p{L}\p{N}_/-])`)

	rows, err := s.Index.DB.Query("SELECT DISTINCT note FROM tags WHERE tag=?", old)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var rels []string
	for rows.Next() {
		var rel string
		if rows.Scan(&rel) == nil {
			rels = append(rels, rel)
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows.Close()

	affected, skipped := 0, 0
	for _, rel := range rels {
		// Renaming a tag rewrites the BODY of every note carrying it. Without
		// this check one request edited the whole vault — every space, every
		// restricted document — on behalf of anyone who could reach the port.
		// Notes the caller cannot write are skipped and counted rather than
		// refusing the whole request, because a tag is legitimately shared
		// across spaces and the alternative is that it can never be renamed.
		if !s.canWrite(r, normPath(rel)) {
			skipped++
			continue
		}
		note, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		body := note.Body
		if !note.Encrypted {
			body = pat.ReplaceAllString(body, "${1}#"+nw+"${2}")
		}
		fm := note.Frontmatter.Clone()
		if v, ok := fm.Get("tags"); ok {
			switch t := v.(type) {
			case []markdown.Value:
				updated := make([]markdown.Value, len(t))
				for i, item := range t {
					if str, ok := item.(string); ok && str == old {
						updated[i] = nw
					} else {
						updated[i] = item
					}
				}
				fm.Set("tags", updated)
			case string:
				if t == old {
					fm.Set("tags", nw)
				}
			}
		}
		if _, err := s.Vault.Write(rel, body, fm); err != nil {
			continue
		}
		if _, err := s.Index.Upsert(rel); err != nil {
			continue
		}
		affected++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"renamed": old, "to": nw, "notes": affected, "skipped": skipped,
	})
}

// graph serves the note graph for the visualiser.
func (s *Server) graph(w http.ResponseWriter, r *http.Request) {
	// Each of these reads used the `if rows, err := ...; err == nil` form,
	// which drops the error and leaves the bucket empty. A failing query then
	// rendered as a 200 with an empty graph — indistinguishable, in the
	// visualiser, from a vault with no notes in it.
	// The graph is a map of the vault, so it is drawn from the caller's
	// readable spaces only. An edge into a space they cannot see would show
	// them a note's existence and its title.
	compact := r.URL.Query().Get("compact") == "1"
	index := map[string]int{}
	ids := []string{}
	titles := []string{}
	times := []int64{}
	if err := s.eachRow("SELECT path, title, acl, COALESCE(created,''), COALESCE(mtime,0) FROM notes", nil, func(rows *sql.Rows) error {
		var path, title, acl, created string
		var mtime float64
		if err := rows.Scan(&path, &title, &acl, &created, &mtime); err != nil {
			return err
		}
		if !s.canReadNote(r, path, acl) {
			return nil
		}
		index[path] = len(ids)
		ids = append(ids, path)
		titles = append(titles, title)
		times = append(times, noteTime(created, mtime))
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Tags let the console filter and name clusters; a handful per note is
	// plenty and keeps the payload small.
	tags := make([][]string, len(ids))
	if err := s.eachRow("SELECT note, tag FROM tags ORDER BY note, tag", nil, func(rows *sql.Rows) error {
		var note, tag string
		if err := rows.Scan(&note, &tag); err != nil {
			return err
		}
		if i, ok := index[note]; ok && len(tags[i]) < 6 {
			tags[i] = append(tags[i], tag)
		}
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	edges := make([]int32, 0, 2*len(ids))
	if err := s.eachRow("SELECT src, dst FROM links WHERE resolved=1", nil,
		func(rows *sql.Rows) error {
			var src, dst string
			if err := rows.Scan(&src, &dst); err != nil {
				return err
			}
			a, okA := index[src]
			b, okB := index[dst]
			if !okA || !okB {
				return nil
			}
			edges = append(edges, int32(a), int32(b))
			return nil
		}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// A link target is text the note that wrote it chose, so it is reported
	// only when that note is one the caller may read. Filtering here by
	// readable nodes alone is not enough: this list is not an edge, and an
	// unresolved [[target]] in a private space was being returned to everyone.
	aclByPath := map[string]string{}
	if err := s.eachRow("SELECT path, acl FROM notes", nil, func(rows *sql.Rows) error {
		var path, acl string
		if err := rows.Scan(&path, &acl); err != nil {
			return err
		}
		aclByPath[path] = acl
		return nil
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	unresolved := []string{}
	seenTarget := map[string]bool{}
	if err := s.eachRow(
		"SELECT src, target FROM links WHERE resolved=0 ORDER BY target", nil,
		func(rows *sql.Rows) error {
			var src, t string
			if err := rows.Scan(&src, &t); err != nil {
				return err
			}
			if seenTarget[t] || len(unresolved) >= 200 || !s.canReadNote(r, src, aclByPath[src]) {
				return nil
			}
			seenTarget[t] = true
			unresolved = append(unresolved, t)
			return nil
		}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var out any
	if compact {
		// Columnar: parallel arrays and one flat edge list of index pairs. A
		// fraction of the object form at 30k edges, and the client reads it
		// straight into typed arrays.
		out = map[string]any{"v": 2, "ids": ids, "titles": titles, "t": times, "tags": tags, "edges": edges, "unresolved": unresolved}
	} else {
		nodes := make([]map[string]string, len(ids))
		for i := range ids {
			nodes[i] = map[string]string{"id": ids[i], "title": titles[i]}
		}
		pairs := make([]map[string]string, 0, len(edges)/2)
		for i := 0; i+1 < len(edges); i += 2 {
			pairs = append(pairs, map[string]string{"src": ids[edges[i]], "dst": ids[edges[i+1]]})
		}
		out = map[string]any{"nodes": nodes, "edges": pairs, "unresolved": unresolved}
	}
	writeCachedJSON(w, r, out)
}

// noteTime is a note's date for the graph's time slider: its frontmatter
// `created` when it parses, otherwise the file's modification time.
func noteTime(created string, mtime float64) int64 {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(created)); err == nil {
			return t.Unix()
		}
	}
	return int64(mtime)
}

// writeCachedJSON answers with a content hash as its ETag so a console that
// already holds the current graph gets a 304 and no body. `no-cache` (not
// `no-store`) keeps the copy but forces revalidation, which matters because
// the graph is per-caller: what one person may see another may not.
func writeCachedJSON(w http.ResponseWriter, r *http.Request, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	sum := sha1.Sum(buf.Bytes())
	etag := `"` + hex.EncodeToString(sum[:8]) + `"`
	h := w.Header()
	h.Set("ETag", etag)
	h.Set("Cache-Control", "private, no-cache")
	h.Add("Vary", "Accept-Encoding")
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Type", "application/json")
	if buf.Len() > 1024 && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		zw := gzip.NewWriter(w)
		_, _ = zw.Write(buf.Bytes())
		_ = zw.Close()
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// eachRow runs a query and hands each row to fn, taking care of the three
// things every one of these loops has to get right and several did not: the
// rows are always closed, a scan failure stops the loop instead of silently
// skipping a row, and rows.Err() is consulted so an iteration that dies
// halfway is reported rather than returned as a short result.
func (s *Server) eachRow(query string, args []any, fn func(*sql.Rows) error) error {
	rows, err := s.Index.DB.Query(query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// tasks lists every `- [ ]` / `- [x]` across the vault, open ones first.
// Encrypted notes are skipped: ciphertext has no parseable tasks.
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	includeDone := truthy(r.URL.Query().Get("include_done"))

	// Tasks are index rows now. This used to read every note body in the
	// vault on every request and scan it for checkboxes, which is linear in
	// the whole vault per call and cannot be narrowed in SQL — so "the open
	// tasks in this project" had to fetch everything and throw most of it
	// away.
	q := index.BlockQuery{
		Filter: filterFor(r, false),
		Kind:   markdown.KindTask,
		Path:   normPath(r.URL.Query().Get("path")),
		Text:   strings.TrimSpace(r.URL.Query().Get("q")),
		Limit:  clampLimit(r.URL.Query().Get("limit"), 500, 2000),
	}
	if !includeDone {
		open := false
		q.Checked = &open
	}
	blocks, err := s.Index.Blocks(q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type task struct {
		Path    string `json:"path"`
		Title   string `json:"title"`
		Line    int    `json:"line"`
		Text    string `json:"text"`
		Done    bool   `json:"done"`
		Section string `json:"section,omitempty"`
		// A task list is note content: an issue in a connected tracker can
		// contain "- [ ] drop the prod database", and it would appear here
		// beside a person's own todos with nothing to tell them apart.
		Origin string `json:"origin,omitempty"`
		Trust  string `json:"trust"`
	}
	out := []task{}
	for _, b := range blocks {
		out = append(out, task{b.Note, b.Title, b.Line, b.Text, b.Checked, b.Parent,
			b.Origin, b.Trust})
	}
	// Open first, then in document order, which is what the console renders.
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Done && out[j].Done })
	writeJSON(w, http.StatusOK, out)
}

// blocks lists the lines inside notes — headings, list items and tasks.
//
// The addressable unit below a note. A heading is how someone finds the
// section they meant; a list item is what a query block counts; and both were
// previously reachable only by reading the note.
func (s *Server) blocks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind := strings.TrimSpace(q.Get("kind"))
	switch kind {
	case "", markdown.KindHeading, markdown.KindItem, markdown.KindTask:
	default:
		writeErr(w, http.StatusBadRequest, "kind must be heading, item or task")
		return
	}
	bq := index.BlockQuery{
		Filter:  filterFor(r, false),
		Kind:    kind,
		Note:    normPath(q.Get("note")),
		Path:    normPath(q.Get("path")),
		Text:    strings.TrimSpace(q.Get("q")),
		Section: strings.TrimSpace(q.Get("section")),
		Limit:   clampLimit(q.Get("limit"), 200, 2000),
	}
	if v := strings.TrimSpace(q.Get("level")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			writeErr(w, http.StatusBadRequest, "level must be a positive number")
			return
		}
		bq.Level = n
	}
	if v := strings.TrimSpace(q.Get("checked")); v != "" {
		checked := truthy(v)
		bq.Checked = &checked
	}
	blocks, err := s.Index.Blocks(bq)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, blocks)
}

// complete backs the `[[` autocomplete: note titles and stems matching a query.
func (s *Server) complete(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	limit := 12
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	like := "%" + q + "%"
	where, spaceArgs := s.whereSpace(r, "space", " WHERE (lower(title) LIKE ? OR lower(path) LIKE ?)")
	args := append(append([]any{like, like}, spaceArgs...), limit)
	rows, err := s.Index.DB.Query(
		"SELECT path, title, acl FROM notes"+where+" ORDER BY updated DESC, path LIMIT ?", args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var path, title, acl string
		if err := rows.Scan(&path, &title, &acl); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !s.canReadNote(r, path, acl) {
			continue
		}
		stem := path
		if i := strings.LastIndex(stem, "/"); i >= 0 {
			stem = stem[i+1:]
		}
		out = append(out, map[string]string{
			"path": path, "title": title, "stem": strings.TrimSuffix(stem, ".md"),
		})
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}
