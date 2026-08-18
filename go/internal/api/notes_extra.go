package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The rest of the note surface: pin, rename, duplicate, link, unlinked
// mentions and version history. Port of the remainder of
// server/routers/notes.py.

// togglePin flips a note's pinned frontmatter flag and returns the new state.
func (s *Server) togglePin(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	fm := note.Frontmatter.Clone()
	pinned := !fm.BoolVal("pinned")
	if pinned {
		fm.Set("pinned", true)
	} else {
		fm.Delete("pinned")
	}
	// write the body back verbatim — for an encrypted note that is the
	// ciphertext, so pinning needs no unlock: only frontmatter changes
	if _, err := s.Vault.Write(rel, note.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "pinned": pinned})
}

// the console sends {to: "new/path.md"} — matching that field name is the
// whole contract, since a mismatch just looks like "rename does nothing"
type renameIn struct {
	To      string `json:"to"`
	Path    string `json:"path"`
	NewPath string `json:"new_path"`
}

// renameNote moves a note and reindexes both paths.
func (s *Server) renameNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	var in renameIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	target := in.To
	if target == "" {
		target = in.NewPath
	}
	if target == "" {
		target = in.Path
	}
	if target == "" {
		writeErr(w, http.StatusBadRequest, "new_path required")
		return
	}
	newRel, err := s.Vault.Rename(rel, normPath(target))
	if err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if err := s.Index.Remove(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := s.Index.Upsert(newRel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"path": newRel})
}

// duplicateNote copies a note to a free path.
func (s *Server) duplicateNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	// the copy is named from the TITLE, not the old path: a note titled
	// "Scratch" at inbox/2026-01-01.md duplicates to scratch-copy.md, which is
	// what a person looking at the list expects to find
	title := fm2Title(note)
	newRel, err := s.uniquePath(vault.Slugify(title) + ".md")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fm := note.Frontmatter.Clone()
	fm.Delete("created")
	fm.Delete("updated")
	fm.Set("title", title)
	if _, err := s.Vault.Write(newRel, note.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	created, err := s.Index.Upsert(newRel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.viewOf(created))
}

// fm2Title is the duplicate's title: the frontmatter title when there is one,
// else the note's derived title, with " (copy)" appended.
func fm2Title(note *vault.Note) string {
	t := note.Frontmatter.StringVal("title")
	if t == "" {
		t = note.Title
	}
	return t + " (copy)"
}

type linkIn struct {
	Source string `json:"source"`
	Name   string `json:"name"`
}

// linkNote turns an unlinked mention into a real link: it wraps the FIRST
// occurrence of `name` in the SOURCE note with a wiki-link to this note.
//
// Note the direction — this edits the mentioning note, not the mentioned one.
func (s *Server) linkNote(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	var in linkIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	var targetTitle string
	if err := s.Index.DB.QueryRow(
		"SELECT title FROM notes WHERE path=?", rel).Scan(&targetTitle); err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	src := normPath(in.Source)
	note, err := s.Vault.Read(src)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such source note")
		return
	}
	if note.Encrypted {
		writeErr(w, http.StatusBadRequest, "cannot edit an encrypted note")
		return
	}
	link := "[[" + in.Name + "]]"
	if !strings.EqualFold(in.Name, targetTitle) {
		link = "[[" + targetTitle + "|" + in.Name + "]]"
	}
	re := mentionRE(in.Name)
	loc := re.FindStringSubmatchIndex(note.Body)
	if loc == nil {
		writeErr(w, http.StatusNotFound, "mention not found")
		return
	}
	// keep the boundary characters the pattern had to consume (RE2 has no
	// lookaround), replacing only the name itself
	before := note.Body[:loc[0]] + note.Body[loc[2]:loc[3]]
	after := note.Body[loc[4]:loc[5]] + note.Body[loc[1]:]
	newBody := before + link + after

	if _, err := s.Vault.Write(src, newBody, note.Frontmatter); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(src); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"linked": src, "count": 1})
}

// mentionRE builds a whole-word match that is NOT already inside a [[wiki-link]].
//
// RE2 has no lookaround, so the guard characters are matched explicitly and the
// caller checks the captured boundaries instead.
func mentionRE(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)(^|[^\p{L}\p{N}_\[])` +
		regexp.QuoteMeta(name) + `($|[^\p{L}\p{N}_\]])`)
}

// unlinkedMentions finds notes that mention this note's title or aliases as
// plain text without linking it.
func (s *Server) unlinkedMentions(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	var title string
	if err := s.Index.DB.QueryRow("SELECT title FROM notes WHERE path=?", rel).Scan(&title); err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	names, err := s.namesFor(rel, title)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(names) == 0 {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	// Swallowing this error would silently widen the result: notes that DO
	// already link here would stop being excluded and get reported as
	// unlinked mentions.
	linked := map[string]bool{}
	if err := s.eachRow("SELECT src FROM links WHERE dst=?", []any{rel},
		func(rows *sql.Rows) error {
			var src string
			if err := rows.Scan(&src); err != nil {
				return err
			}
			linked[src] = true
			return nil
		}); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type pat struct {
		name string
		re   *regexp.Regexp
	}
	pats := make([]pat, 0, len(names))
	for _, n := range names {
		pats = append(pats, pat{n, mentionRE(n)})
	}

	rows, err := s.Index.DB.Query("SELECT path, title, body FROM notes WHERE path != ?", rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var path, rtitle, body string
		if err := rows.Scan(&path, &rtitle, &body); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if linked[path] || vault.IsEncrypted(body) {
			continue
		}
		for _, p := range pats {
			loc := p.re.FindStringIndex(body)
			if loc == nil {
				continue
			}
			start := strings.LastIndex(body[:loc[0]], "\n") + 1
			end := strings.Index(body[loc[1]:], "\n")
			var snippet string
			if end == -1 {
				snippet = body[start:]
			} else {
				snippet = body[start : loc[1]+end]
			}
			snippet = strings.TrimSpace(snippet)
			if len(snippet) > 160 {
				snippet = snippet[:160]
			}
			out = append(out, map[string]string{
				"path": path, "title": rtitle, "name": p.name, "context": snippet,
			})
			break
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// namesFor returns the title plus aliases, dropping anything too short to be a
// meaningful mention.
func (s *Server) namesFor(rel, title string) ([]string, error) {
	names := []string{title}
	var fmJSON string
	if err := s.Index.DB.QueryRow(
		"SELECT frontmatter_json FROM notes WHERE path=?", rel).Scan(&fmJSON); err == nil {
		var m map[string]any
		if json.Unmarshal([]byte(fmJSON), &m) == nil {
			switch a := m["aliases"].(type) {
			case string:
				names = append(names, a)
			case []any:
				for _, x := range a {
					if str, ok := x.(string); ok {
						names = append(names, str)
					}
				}
			}
		}
	}
	seen := map[string]bool{}
	out := []string{}
	for _, n := range names {
		if len(n) < 3 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// ---------------------------------------------------------------- history

func (s *Server) noteHistory(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	var one int
	if err := s.Index.DB.QueryRow("SELECT 1 FROM notes WHERE path=?", rel).Scan(&one); err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	writeJSON(w, http.StatusOK, s.History.ListVersions(rel))
}

func (s *Server) noteHistoryVersion(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	id := r.PathValue("version")
	body, ok := s.History.GetVersion(rel, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such version")
		return
	}
	if vault.IsEncrypted(body) {
		// never hand back raw ciphertext: answer 423 so the console prompts
		writeErr(w, http.StatusLocked, "vault locked — unlock to view this version")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id, "body": body})
}

// restoreVersion puts an old body back. The current body is snapshotted first,
// so a restore is itself undoable.
func (s *Server) restoreVersion(w http.ResponseWriter, r *http.Request) {
	rel := normPath(r.PathValue("path"))
	id := r.PathValue("version")
	body, ok := s.History.GetVersion(rel, id)
	if !ok {
		writeErr(w, http.StatusNotFound, "no such version")
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such note")
		return
	}
	s.History.Snapshot(rel, note.Body)
	if _, err := s.Vault.Write(rel, body, note.Frontmatter); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	restored, err := s.Index.Upsert(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.viewOf(restored))
}

// ---------------------------------------------------------------- templates

func (s *Server) listTemplates(w http.ResponseWriter, _ *http.Request) {
	rels, err := s.Vault.WalkDir("templates")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := []map[string]string{}
	for _, rel := range rels {
		note, err := s.Vault.Read(rel)
		if err != nil {
			continue
		}
		// the console reads `name`, not `title`
		out = append(out, map[string]string{"path": rel, "name": note.Title})
	}
	writeJSON(w, http.StatusOK, out)
}

type templateApply struct {
	Template string `json:"template"`
	Title    string `json:"title"`
}

// applyTemplate creates a note from a template, substituting {{date}}, {{time}}
// and {{title}}.
func (s *Server) applyTemplate(w http.ResponseWriter, r *http.Request) {
	var in templateApply
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	tpl, err := s.Vault.Read(normPath(in.Template))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such template")
		return
	}
	now := vault.Now()
	title := in.Title
	if title == "" {
		title = "Untitled"
	}
	body := strings.NewReplacer(
		"{{date}}", now.Format("2006-01-02"),
		"{{time}}", now.Format("15:04"),
		"{{datetime}}", now.Format("2006-01-02 15:04"),
		"{{title}}", title,
	).Replace(tpl.Body)

	rel, err := s.uniquePath(vault.Slugify(title) + ".md")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", title)
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel, "title": title})
}
