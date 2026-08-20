package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/render"
)

// Publishing — a public read-only site cut from the notes you marked.
//
// Obsidian's answer to this is a hosted service. The self-hosted answer has to
// be a surface on the server you already run, and the whole difficulty is that
// it is the one surface with no principal behind it: every other read here
// answers "may THIS caller see it", and this one answers "did the author say
// this is public".
//
// So it is deliberately narrow, and every part of it is a refusal:
//
//   - it does not exist unless the operator turned it on. A public surface
//     must not appear because somebody typed a frontmatter key;
//   - a note is served only if it says publish: true, is not private, is not
//     encrypted, and is not in the trash;
//   - links resolve against PUBLISHED notes only, so a link to an unpublished
//     note renders as unresolved rather than as a working URL into the vault;
//   - backlinks come only from published notes, so an unpublished note cannot
//     announce itself by linking to a published one;
//   - the global auth token still gates it. An operator who closed the server
//     closed it; a "public" surface that punched through that would be a hole,
//     and the way to run a public site is not to set that token.

// PublishSetting is the operator's switch.
const PublishSetting = "publish"

// publishEnabled reports whether the public surface exists at all.
func (s *Server) publishEnabled() bool {
	if s.Settings == nil {
		return false
	}
	return truthy(s.Settings.Get(PublishSetting))
}

// publishedWhere is the only definition of "published", used by every query
// here so the surface cannot disagree with itself about what it serves.
//
// The frontmatter JSON is matched the way the briefing matches pinned: the
// index stores it with a space after the colon, and a pattern without one
// silently matches nothing — which on this surface would look like "you have
// published nothing" rather than like a bug.
const publishedWhere = `private = 0 AND frontmatter_json LIKE '%"publish": true%'`

// publishedPaths is the link map the renderer resolves against: published
// notes only.
func (s *Server) publishedPaths() (map[string]string, error) {
	rows, err := s.Index.DB.Query(
		"SELECT path, title FROM notes WHERE " + publishedWhere)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, title string
		if err := rows.Scan(&path, &title); err != nil {
			return nil, err
		}
		// The shapes a wiki-link is written in: title, path, and stem.
		setDefault(out, strings.ToLower(title), path)
		setDefault(out, strings.ToLower(path), path)
		setDefault(out, strings.ToLower(stripMD(path)), path)
		if i := strings.LastIndex(stripMD(path), "/"); i >= 0 {
			setDefault(out, strings.ToLower(stripMD(path)[i+1:]), path)
		}
	}
	return out, rows.Err()
}

func setDefault(m map[string]string, key, value string) {
	if key == "" {
		return
	}
	if _, taken := m[key]; !taken {
		m[key] = value
	}
}

type publishedNote struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Updated string `json:"updated"`
}

func (s *Server) publishedList() ([]publishedNote, error) {
	rows, err := s.Index.DB.Query(
		"SELECT path, title, updated FROM notes WHERE " + publishedWhere +
			" ORDER BY title, path")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []publishedNote{}
	for rows.Next() {
		var n publishedNote
		if err := rows.Scan(&n.Path, &n.Title, &n.Updated); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// publishedIndex lists the published notes.
func (s *Server) publishedIndex(w http.ResponseWriter, r *http.Request) {
	if !s.publishEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	notes, err := s.publishedList()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var items strings.Builder
	for _, n := range notes {
		items.WriteString(fmt.Sprintf(`<a href="/published/%s">%s</a>`,
			stripMD(n.Path), htmlEscape(n.Title)))
	}
	if items.Len() == 0 {
		items.WriteString("<p>Nothing published yet.</p>")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readPage("Published", "<h1>Published</h1><nav>"+items.String()+"</nav>"))
}

// publishedAPI is the same list as JSON, for anything building its own site.
func (s *Server) publishedAPI(w http.ResponseWriter, r *http.Request) {
	if !s.publishEnabled() {
		writeErr(w, http.StatusNotFound, "publishing is not enabled")
		return
	}
	notes, err := s.publishedList()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, notes)
}

// publishedNotePage serves one published note.
func (s *Server) publishedNotePage(w http.ResponseWriter, r *http.Request) {
	if !s.publishEnabled() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	rel := normPath(r.PathValue("path"))
	if !strings.HasSuffix(rel, ".md") {
		rel += ".md"
	}
	var title, body string
	// Not "is this note published AND may the caller read it" — there is no
	// caller here. Published is the whole check, which is why it is one
	// constant used everywhere rather than a condition retyped per handler.
	err := s.Index.DB.QueryRow(
		"SELECT title, body FROM notes WHERE path=? AND "+publishedWhere, rel).
		Scan(&title, &body)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	linkMap, err := s.publishedPaths()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rendered := render.RenderWith(body, &render.Context{
		LinkMap:  linkMap,
		LinkHref: func(rel string) string { return "/published/" + stripMD(rel) },
	})

	back := ""
	links, err := s.publishedBacklinks(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(links) > 0 {
		var rendered strings.Builder
		for _, b := range links {
			rendered.WriteString(fmt.Sprintf(`<a href="/published/%s">%s</a> `,
				stripMD(b.Path), htmlEscape(b.Title)))
		}
		back = "<hr><p class='back'>Linked from: " + rendered.String() + "</p>"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readPage(title,
		`<p class="back"><a href="/published">← published notes</a></p>`+rendered+back))
}

// publishedBacklinks finds published notes linking here. An unpublished note
// linking to a published one must not be able to announce itself in its
// footer.
func (s *Server) publishedBacklinks(rel string) ([]publishedNote, error) {
	rows, err := s.Index.DB.Query(
		"SELECT DISTINCT n.path, n.title, n.updated FROM links l JOIN notes n ON n.path = l.src "+
			"WHERE l.dst = ? AND l.resolved = 1 AND "+publishedWhere+" ORDER BY n.title", rel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []publishedNote{}
	for rows.Next() {
		var n publishedNote
		if err := rows.Scan(&n.Path, &n.Title, &n.Updated); err != nil {
			return nil, err
		}
		if n.Path == rel {
			continue
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
