package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Facts, tag rename, graph, tasks and completion.
// Port of the remainder of server/routers/search.py and misc.py.

var taskLineRE = regexp.MustCompile(`^\s*[-*]\s+\[([ xX])\]\s+(.*)$`)

// facts serves the structured `key:: value` layer projected from note bodies —
// a deterministic lookup over the same markdown. Private notes' facts are
// excluded unless explicitly included.
func (s *Server) facts(w http.ResponseWriter, r *http.Request) {
	var conds []string
	var args []any
	if k := strings.TrimSpace(r.URL.Query().Get("key")); k != "" {
		conds = append(conds, "key=?")
		args = append(args, strings.ToLower(k))
	}
	if n := strings.TrimSpace(r.URL.Query().Get("note")); n != "" {
		conds = append(conds, "note=?")
		args = append(args, n)
	}
	if !truthy(r.URL.Query().Get("include_private")) {
		conds = append(conds, "private=0")
	}
	q := "SELECT note, key, value FROM facts"
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY key, note"

	rows, err := s.Index.DB.Query(q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var note, key, value string
		if err := rows.Scan(&note, &key, &value); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		out = append(out, map[string]string{"note": note, "key": key, "value": value})
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
	rows.Close()

	affected := 0
	for _, rel := range rels {
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
		"renamed": old, "to": nw, "notes": affected,
	})
}

// graph serves the note graph for the visualiser.
func (s *Server) graph(w http.ResponseWriter, _ *http.Request) {
	nodes := []map[string]string{}
	if rows, err := s.Index.DB.Query("SELECT path, title FROM notes"); err == nil {
		for rows.Next() {
			var path, title string
			if rows.Scan(&path, &title) == nil {
				nodes = append(nodes, map[string]string{"id": path, "title": title})
			}
		}
		rows.Close()
	}
	edges := []map[string]string{}
	if rows, err := s.Index.DB.Query(
		"SELECT src, dst FROM links WHERE resolved=1"); err == nil {
		for rows.Next() {
			var src, dst string
			if rows.Scan(&src, &dst) == nil {
				edges = append(edges, map[string]string{"src": src, "dst": dst})
			}
		}
		rows.Close()
	}
	unresolved := []string{}
	if rows, err := s.Index.DB.Query(
		"SELECT DISTINCT target FROM links WHERE resolved=0 ORDER BY target LIMIT 200"); err == nil {
		for rows.Next() {
			var t string
			if rows.Scan(&t) == nil {
				unresolved = append(unresolved, t)
			}
		}
		rows.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"nodes": nodes, "edges": edges, "unresolved": unresolved,
	})
}

// tasks lists every `- [ ]` / `- [x]` across the vault, open ones first.
// Encrypted notes are skipped: ciphertext has no parseable tasks.
func (s *Server) tasks(w http.ResponseWriter, r *http.Request) {
	includeDone := truthy(r.URL.Query().Get("include_done"))
	rows, err := s.Index.DB.Query(
		"SELECT path, title, body FROM notes ORDER BY updated DESC, path")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type task struct {
		Path  string `json:"path"`
		Title string `json:"title"`
		Line  int    `json:"line"`
		Text  string `json:"text"`
		Done  bool   `json:"done"`
	}
	out := []task{}
	for rows.Next() {
		var path, title, body string
		if err := rows.Scan(&path, &title, &body); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		for i, line := range strings.Split(body, "\n") {
			m := taskLineRE.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			done := strings.ToLower(m[1]) == "x"
			if done && !includeDone {
				continue
			}
			out = append(out, task{path, title, i, strings.TrimSpace(m[2]), done})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].Done && out[j].Done })
	writeJSON(w, http.StatusOK, out)
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
	rows, err := s.Index.DB.Query(
		"SELECT path, title FROM notes WHERE lower(title) LIKE ? "+
			"OR lower(path) LIKE ? ORDER BY updated DESC, path LIMIT ?", like, like, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		stem := path
		if i := strings.LastIndex(stem, "/"); i >= 0 {
			stem = stem[i+1:]
		}
		out = append(out, map[string]string{
			"path": path, "title": title, "stem": strings.TrimSuffix(stem, ".md"),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
