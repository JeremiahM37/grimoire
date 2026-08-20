package api

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// blockQueryFor finds the heading a bookmark names, inside the caller's
// visibility.
func blockQueryFor(r *http.Request, path, anchor string) index.BlockQuery {
	return index.BlockQuery{
		Filter: filterFor(r, true), Kind: markdown.KindHeading,
		Note: path, Text: anchor, Limit: 1,
	}
}

// Bookmarks — the things you keep coming back to.
//
// Pinning already covered notes, which is the easy half. The half that was
// missing is everything that is not a note: the section of a long runbook you
// actually use, and the search you re-type every Monday. Both are addressable
// — a heading is an indexed block, a search is a string the search box already
// understands — so neither needed a new concept, only somewhere to keep them.
//
// They live in a note. A bookmarks file that lives in the index would be lost
// on a reindex and invisible to the sync that carries the vault to a phone;
// as a note it is a file you own, it syncs, and you can edit it in the editor
// like anything else — which is the same argument as memory being markdown.

// BookmarksNote is where they are kept.
const BookmarksNote = "bookmarks.md"

// Kinds of bookmark. A search is stored as the query text the search box
// takes, so a bookmark is exactly what you would have typed.
const (
	bookmarkNote    = "note"
	bookmarkHeading = "heading"
	bookmarkSearch  = "search"
	bookmarkTag     = "tag"
)

var bookmarkKinds = map[string]bool{
	bookmarkNote: true, bookmarkHeading: true, bookmarkSearch: true, bookmarkTag: true,
}

// bookmarkRE parses one stored line. The target is wrapped in [[…]] for the
// things that are links and in backticks for the things that are text, so the
// file reads as markdown rather than as a database someone spilled into a note.
var bookmarkRE = regexp.MustCompile("^-\\s+\\*\\*([a-z]+)\\*\\*\\s+—\\s+(?:\\[\\[(.+?)\\]\\]|`(.+?)`)\\s*$")

type bookmark struct {
	Kind   string `json:"kind"`
	Target string `json:"target"`
	// Label is what the console shows. Derived, not stored: the note's title
	// can change, and a stored copy would go stale silently.
	Label string `json:"label,omitempty"`
	// Path and Line locate a heading bookmark, so following one lands on the
	// section rather than at the top of a long note.
	Path string `json:"path,omitempty"`
	Line int    `json:"line,omitempty"`
}

func (b bookmark) line() string {
	if b.Kind == bookmarkSearch || b.Kind == bookmarkTag {
		return "- **" + b.Kind + "** — `" + b.Target + "`"
	}
	return "- **" + b.Kind + "** — [[" + b.Target + "]]"
}

func parseBookmarks(body string) []bookmark {
	var out []bookmark
	for _, line := range strings.Split(body, "\n") {
		m := bookmarkRE.FindStringSubmatch(strings.TrimRight(line, " \t"))
		if m == nil || !bookmarkKinds[m[1]] {
			continue
		}
		target := m[2]
		if target == "" {
			target = m[3]
		}
		out = append(out, bookmark{Kind: m[1], Target: strings.TrimSpace(target)})
	}
	return out
}

// listBookmarks returns what is bookmarked, resolved.
func (s *Server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	marks, err := s.readBookmarks(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, marks)
}

func (s *Server) readBookmarks(r *http.Request) ([]bookmark, error) {
	note, err := s.Vault.Read(BookmarksNote)
	if err != nil {
		return []bookmark{}, nil // nothing bookmarked yet is not an error
	}
	marks := parseBookmarks(note.Body)
	out := make([]bookmark, 0, len(marks))
	for _, b := range marks {
		resolved, ok := s.resolveBookmark(r, b)
		if !ok {
			// A bookmark pointing at a note this caller cannot read is not
			// shown to them. The line stays in the file — it is somebody's
			// bookmark, and the reader is not necessarily its owner.
			continue
		}
		out = append(out, resolved)
	}
	return out, nil
}

// resolveBookmark fills in what a bookmark points at, and reports whether the
// caller may see it at all.
func (s *Server) resolveBookmark(r *http.Request, b bookmark) (bookmark, bool) {
	switch b.Kind {
	case bookmarkSearch, bookmarkTag:
		b.Label = b.Target
		return b, true
	}
	target, anchor, _ := strings.Cut(b.Target, "#")
	path, title := s.resolveNoteTarget(target)
	if path == "" {
		b.Label = b.Target // dangling, like an unresolved wiki-link
		return b, true
	}
	if !s.canRead(r, path) {
		return b, false
	}
	b.Path = path
	b.Label = title
	if anchor != "" {
		b.Label = title + " › " + anchor
		blocks, err := s.Index.Blocks(blockQueryFor(r, path, anchor))
		if err == nil && len(blocks) > 0 {
			b.Line = blocks[0].Line
		}
	}
	return b, true
}

// resolveNoteTarget finds a note by path, title or stem — the same shapes a
// wiki-link accepts, so a bookmark is written the way a link is.
func (s *Server) resolveNoteTarget(target string) (path, title string) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", ""
	}
	candidates := []string{target}
	if !strings.HasSuffix(target, ".md") {
		candidates = append(candidates, target+".md")
	}
	for _, candidate := range candidates {
		var p, t string
		if err := s.Index.DB.QueryRow(
			"SELECT path, title FROM notes WHERE path=?", candidate).Scan(&p, &t); err == nil {
			return p, t
		}
	}
	var p, t string
	if err := s.Index.DB.QueryRow(
		"SELECT path, title FROM notes WHERE title=? COLLATE NOCASE ORDER BY path LIMIT 1",
		target).Scan(&p, &t); err == nil {
		return p, t
	}
	return "", ""
}

// addBookmark records one, refusing a duplicate rather than growing the file.
func (s *Server) addBookmark(w http.ResponseWriter, r *http.Request) {
	var in bookmark
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.Kind = strings.ToLower(strings.TrimSpace(in.Kind))
	in.Target = strings.TrimSpace(in.Target)
	if !bookmarkKinds[in.Kind] {
		writeErr(w, http.StatusBadRequest, "kind must be note, heading, search or tag")
		return
	}
	if in.Target == "" || len(in.Target) > 500 {
		writeErr(w, http.StatusBadRequest, "target must be 1..500 characters")
		return
	}
	// The target is rendered into a markdown line, so it must not be able to
	// end that line and write another one — or to close the [[ ]] it sits in.
	if strings.ContainsAny(in.Target, "\n\r") || strings.Contains(in.Target, "]]") ||
		strings.Contains(in.Target, "`") {
		writeErr(w, http.StatusBadRequest, "target contains markup that cannot be stored")
		return
	}
	if !s.requireWrite(w, r, BookmarksNote) {
		return
	}
	// A bookmark to a note nobody may read would be a way to probe for one.
	if in.Kind == bookmarkNote || in.Kind == bookmarkHeading {
		target, _, _ := strings.Cut(in.Target, "#")
		if path, _ := s.resolveNoteTarget(target); path != "" && !s.canRead(r, path) {
			writeErr(w, http.StatusNotFound, "no such note")
			return
		}
	}

	existing, err := s.Vault.Read(BookmarksNote)
	var body string
	var fm *markdown.Frontmatter
	if err == nil {
		for _, have := range parseBookmarks(existing.Body) {
			if have.Kind == in.Kind && strings.EqualFold(have.Target, in.Target) {
				writeJSON(w, http.StatusOK, map[string]any{
					"bookmark": in, "created": false})
				return
			}
		}
		s.History.Snapshot(BookmarksNote, existing.Body)
		fm = existing.Frontmatter.Clone()
		body = strings.TrimRight(existing.Body, "\n") + "\n" + in.line() + "\n"
	} else {
		fm = markdown.NewFrontmatter()
		fm.Set("title", "Bookmarks")
		body = "# Bookmarks\n\n" + in.line() + "\n"
	}
	if _, err := s.Vault.Write(BookmarksNote, body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(BookmarksNote); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	resolved, _ := s.resolveBookmark(r, in)
	writeJSON(w, http.StatusCreated, map[string]any{"bookmark": resolved, "created": true})
}

// removeBookmark drops one line, leaving the rest of the note alone.
func (s *Server) removeBookmark(w http.ResponseWriter, r *http.Request) {
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	target := strings.TrimSpace(r.URL.Query().Get("target"))
	if !bookmarkKinds[kind] || target == "" {
		writeErr(w, http.StatusBadRequest, "kind and target are required")
		return
	}
	if !s.requireWrite(w, r, BookmarksNote) {
		return
	}
	existing, err := s.Vault.Read(BookmarksNote)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such bookmark")
		return
	}
	var kept []string
	found := false
	for _, line := range strings.Split(existing.Body, "\n") {
		if m := bookmarkRE.FindStringSubmatch(strings.TrimRight(line, " \t")); m != nil {
			have := m[2]
			if have == "" {
				have = m[3]
			}
			if m[1] == kind && strings.EqualFold(strings.TrimSpace(have), target) {
				found = true
				continue
			}
		}
		kept = append(kept, line)
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no such bookmark")
		return
	}
	s.History.Snapshot(BookmarksNote, existing.Body)
	if _, err := s.Vault.Write(BookmarksNote, strings.Join(kept, "\n"),
		existing.Frontmatter.Clone()); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(BookmarksNote); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}
