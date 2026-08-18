package api

import (
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/fts"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Full-text search. Port of server/routers/search.py::search.
//
// Three behaviours here are load-bearing and easy to omit, because each only
// shows up under a specific kind of query:
//
//   - the any-term OR fallback. A natural-language question rarely matches
//     EVERY term, so an AND-only query returns nothing for exactly the queries
//     an agent asks. Falling back to any-term is what makes question-shaped
//     search work at all.
//   - full=true. Agents opt into bodies to avoid a search→read round-trip per
//     hit; long notes come back as the query-relevant excerpt rather than the
//     whole note.
//   - the tag:/is:pinned/path: operators, which the console's search box sends.

// excerptBudget is the character budget for a long note under full=true.
const excerptBudget = 2400

// fullBodyThreshold is the size past which a body is excerpted rather than
// returned whole.
const fullBodyThreshold = 2400

type searchHit struct {
	Path      string `json:"path"`
	Title     string `json:"title"`
	Snippet   string `json:"snippet"`
	Body      string `json:"body,omitempty"`
	Excerpted bool   `json:"excerpted,omitempty"`
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	opTag := r.URL.Query().Get("tag")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	full := truthy(r.URL.Query().Get("full"))

	// operators: tag:X  is:pinned  path:X — everything else is full text
	wantPinned := false
	pathLike := ""
	var terms []string
	for _, tok := range strings.Fields(q) {
		low := strings.ToLower(tok)
		switch {
		case strings.HasPrefix(low, "tag:"):
			opTag = tok[4:]
		case low == "is:pinned" || low == "is:pin":
			wantPinned = true
		case strings.HasPrefix(low, "path:"):
			pathLike = strings.ToLower(tok[5:])
		default:
			terms = append(terms, tok)
		}
	}

	type row struct{ path, title, snippet string }
	var rows []row

	if len(terms) > 0 {
		query := "SELECT path, title, snippet(fts, 2, '[', ']', ' … ', 12) AS snippet " +
			"FROM fts WHERE fts MATCH ? ORDER BY bm25(fts) LIMIT 500"
		// The closure returns an error rather than swallowing one. Returning
		// an empty slice on failure made a broken query indistinguishable from
		// a query with no matches, and the caller then "fell back" to a
		// broader search that failed the same way — reporting "no results"
		// for what was actually an index error.
		scan := func(match string) ([]row, error) {
			out := []row{}
			rs, err := s.Index.DB.Query(query, match)
			if err != nil {
				return nil, err
			}
			defer rs.Close()
			for rs.Next() {
				var x row
				if err := rs.Scan(&x.path, &x.title, &x.snippet); err != nil {
					return nil, err
				}
				out = append(out, x)
			}
			return out, rs.Err()
		}
		var err error
		rows, err = scan(fts.PrefixTerms(terms, fts.And))
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if len(rows) == 0 && len(terms) > 1 {
			// natural-language queries rarely match EVERY term — fall back to
			// any-term so a question still surfaces its best notes
			if rows, err = scan(fts.PrefixTerms(terms, fts.Or)); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	} else if opTag != "" || wantPinned || pathLike != "" {
		rs, err := s.Index.DB.Query(
			"SELECT path, title, '' FROM notes ORDER BY updated DESC, path LIMIT 500")
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rs.Close()
		for rs.Next() {
			var x row
			if err := rs.Scan(&x.path, &x.title, &x.snippet); err != nil {
				writeErr(w, http.StatusInternalServerError, err.Error())
				return
			}
			rows = append(rows, x)
		}
		if err := rs.Err(); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	} else {
		writeJSON(w, http.StatusOK, []searchHit{})
		return
	}

	out := []searchHit{}
	for _, x := range rows {
		if opTag != "" {
			var one int
			if s.Index.DB.QueryRow("SELECT 1 FROM tags WHERE note=? AND tag=?",
				x.path, opTag).Scan(&one) != nil {
				continue
			}
		}
		if pathLike != "" && !strings.Contains(strings.ToLower(x.path), pathLike) {
			continue
		}
		if wantPinned && !s.isPinned(x.path) {
			continue
		}
		hit := searchHit{Path: x.path, Title: x.title, Snippet: x.snippet}
		if full {
			var body string
			if s.Index.DB.QueryRow("SELECT body FROM notes WHERE path=?",
				x.path).Scan(&body) != nil {
				body = ""
			}
			switch {
			case vault.IsEncrypted(body):
				// stays sealed: the body is ciphertext, so return nothing
				// rather than noise
				body = ""
			case len(body) > fullBodyThreshold:
				body = excerpt(body, terms)
				hit.Excerpted = true
			}
			hit.Body = body
		}
		out = append(out, hit)
		if len(out) >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) isPinned(path string) bool {
	var fmJSON string
	if s.Index.DB.QueryRow("SELECT frontmatter_json FROM notes WHERE path=?",
		path).Scan(&fmJSON) != nil {
		return false
	}
	return pinnedFlag(fmJSON)
}

// excerpt returns the most query-relevant ~budget characters of a long body:
// score its chunks by how many query terms they contain, keep the best ones in
// DOCUMENT order. With no scoring signal this falls back to the head of the
// note, which is the useful default for an untargeted match.
func excerpt(body string, terms []string) string {
	chunks := embed.ChunkText(body)
	if len(chunks) == 0 {
		return body
	}
	toks := make([]string, len(terms))
	for i, t := range terms {
		toks[i] = strings.ToLower(t)
	}
	idx := make([]int, len(chunks))
	for i := range chunks {
		idx[i] = i
	}
	score := func(i int) int {
		low := strings.ToLower(chunks[i])
		n := 0
		for _, t := range toks {
			if strings.Contains(low, t) {
				n++
			}
		}
		return n
	}
	// stable sort by descending score: equal-scoring chunks keep document
	// order, so an unscored body yields its head
	sort.SliceStable(idx, func(a, b int) bool { return score(idx[a]) > score(idx[b]) })

	keep := map[int]bool{}
	used := 0
	for _, i := range idx {
		if used >= excerptBudget {
			break
		}
		keep[i] = true
		used += len(chunks[i])
	}
	ordered := make([]int, 0, len(keep))
	for i := range keep {
		ordered = append(ordered, i)
	}
	sort.Ints(ordered)
	parts := make([]string, len(ordered))
	for i, c := range ordered {
		parts[i] = chunks[c]
	}
	return strings.Join(parts, "\n[…]\n")
}
