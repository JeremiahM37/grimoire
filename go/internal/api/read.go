package api

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/render"
)

// The e-ink / Kindle read surface — plain HTML, no JS, big fonts, hyperlinked.
// A read-mostly device can browse the whole vault here without the console.
// Private notes are excluded: this surface is not necessarily authenticated.
//
// The markup is byte-matched to server/routers/read.py, including the
// stylesheet. It is a user-visible page under vision test, so "close enough"
// would show up as a changed screenshot.

const readStyle = `<style>
body{font-family:Georgia,serif;max-width:40rem;margin:0 auto;padding:1.2rem;
font-size:1.25rem;line-height:1.7;color:#111;background:#fff}
a{color:#000}h1,h2,h3{line-height:1.25}code{font-family:monospace}
.unresolved{color:#888}nav a{display:block;padding:.3rem 0}
hr{border:none;border-top:1px solid #ccc;margin:1.5rem 0}
img{max-width:100%;height:auto}
mark{background:#fdf3b0}del{color:#888}
.hl-kw{color:#6a4bd8;font-weight:600}.hl-str{color:#2f6f6a}.hl-com{color:#888;font-style:italic}.hl-num{color:#b26b3a}pre code{background:none;padding:0}
.callout{border:1px solid #ddd;border-left:4px solid #888;border-radius:8px;margin:1rem 0;padding:.5rem 1rem}
.callout-title{font-weight:600;margin-bottom:.2rem}
.back{font-size:1rem}
</style>`

func readPage(title, body string) string {
	return "<!doctype html><html><head><meta charset='utf-8'>" +
		"<meta name='viewport' content='width=device-width,initial-scale=1'>" +
		"<title>" + htmlEscape(title) + "</title>" + readStyle +
		"</head><body>" + body + "</body></html>"
}

// stripMD is the link form used throughout the read surface: paths without
// their extension.
func stripMD(path string) string { return strings.TrimSuffix(path, ".md") }

// readIndex lists the public notes this caller may see.
//
// "private=0" was the whole filter, which on a single-user vault is the right
// one and on a multi-user one is none at all: an anonymous visitor could list
// every colleague's notes here, and read them below. The e-ink surface is a
// READ surface, so it answers to the same rules as every other read.
func (s *Server) readIndex(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Index.DB.Query(
		"SELECT path, title, acl FROM notes WHERE private=0 ORDER BY title")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var items strings.Builder
	for rows.Next() {
		var path, title, acl string
		if err := rows.Scan(&path, &title, &acl); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if !s.canReadNote(r, path, acl) {
			continue
		}
		items.WriteString(fmt.Sprintf(`<a href="/read/%s">%s</a>`,
			stripMD(path), htmlEscape(title)))
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readPage("Grimoire — notes",
		"<h1>Grimoire</h1><nav>"+items.String()+"</nav>"))
}

// readNote renders one public note.
func (s *Server) readNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	if !s.canRead(r, rel) {
		// Absent rather than forbidden, like every other read of a note the
		// caller may not see.
		s.auditRead(r, rel, false)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	s.auditRead(r, rel, true)
	var title, body string
	err := s.Index.DB.QueryRow(
		"SELECT title, body FROM notes WHERE path=? AND private=0", rel).Scan(&title, &body)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	linkMap, err := s.linkMap()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rendered := render.RenderWith(body, &render.Context{
		LinkMap:  linkMap,
		LinkHref: func(rel string) string { return "/read/" + stripMD(rel) },
	})

	back := ""
	bl, err := s.backlinks(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(bl) > 0 {
		var links strings.Builder
		for _, b := range bl {
			links.WriteString(fmt.Sprintf(`<a href="/read/%s">%s</a> `,
				stripMD(fmt.Sprint(b["path"])), htmlEscape(fmt.Sprint(b["title"]))))
		}
		back = "<hr><p class='back'>Linked from: " + links.String() + "</p>"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readPage(title,
		`<p class="back"><a href="/read">← all notes</a></p>`+rendered+back))
}

// exportNote renders a note as a self-contained HTML file for download.
func (s *Server) exportNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	var title, body string
	err := s.Index.DB.QueryRow(
		"SELECT title, body FROM notes WHERE path=? AND private=0", rel).Scan(&title, &body)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	linkMap, err := s.linkMap()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	rendered := render.Render(body, linkMap)
	// served INLINE, not as an attachment: the console opens this in a popup to
	// print or save from, and a download response never renders
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, readPage(title, rendered))
}
