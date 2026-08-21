package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Daily notes and the capture inbox. Port of server/routers/daily.py.

var dateRE = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func (s *Server) dailyRel(date string) string {
	if date == "" {
		date = vault.Now().Format("2006-01-02")
	}
	return s.DailyDir + "/" + date + ".md"
}

// daily returns today's (or a given date's) note, creating it if absent.
func (s *Server) daily(w http.ResponseWriter, r *http.Request) {
	date := r.URL.Query().Get("date")
	if date != "" && !dateRE.MatchString(date) {
		writeErr(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
		return
	}
	rel, note, err := s.ensureDaily(date)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"path": rel, "title": note.Title, "body": note.Body,
	})
}

// ensureDaily creates the daily note if it does not exist yet, and returns it.
func (s *Server) ensureDaily(date string) (string, *vault.Note, error) {
	if date == "" {
		date = vault.Now().Format("2006-01-02")
	}
	rel := s.dailyRel(date)
	if note, err := s.Vault.Read(rel); err == nil {
		return rel, note, nil
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", date)
	fm.Set("tags", []markdown.Value{"daily"})
	if _, err := s.Vault.Write(rel, "# "+date+"\n\n", fm); err != nil {
		return rel, nil, err
	}
	note, err := s.Index.Upsert(rel)
	return rel, note, err
}

// dailyDates lists which dates already have a daily note, for the calendar view.
// dailyDates lists the days this caller has a journal entry for.
//
// Filtered in SQL rather than in the loop: the reader-list lookup canRead does
// is a query, and a query issued while this cursor is open waits for the one
// connection the cursor holds.
func (s *Server) dailyDates(w http.ResponseWriter, r *http.Request) {
	where, args := s.whereReadable(r, "space", "acl", " WHERE path LIKE ?")
	rows, err := s.Index.DB.Query("SELECT path FROM notes"+where,
		append([]any{s.DailyDir + "/%"}, args...)...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		stem := path
		if i := strings.LastIndex(stem, "/"); i >= 0 {
			stem = stem[i+1:]
		}
		stem = strings.TrimSuffix(stem, ".md")
		if dateRE.MatchString(stem) {
			out = append(out, stem)
		}
	}
	if err := rows.Err(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type captureIn struct {
	Text   string `json:"text"`
	Title  string `json:"title"`
	URL    string `json:"url"`
	Source string `json:"source"`
}

// capture accepts a note from outside (browser clip, CLI, share). It lands in
// the inbox, and a link is appended to today's daily note so nothing gets lost.
func (s *Server) capture(w http.ResponseWriter, r *http.Request) {
	var c captureIn
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if c.Source == "" {
		c.Source = "capture"
	}
	now := vault.Now()
	stamp := now.Format("20060102-150405")
	title := c.Title
	if title == "" {
		title = "capture " + stamp
	}
	rel := fmt.Sprintf("%s/%s-%s.md", s.InboxDir, stamp, vault.Slugify(title))
	// The inbox is the commons by default, but a space is any path prefix —
	// so whether this caller may write there is a question, not a given.
	if !s.requireWrite(w, r, rel) {
		return
	}

	body := c.Text
	if c.URL != "" {
		body = "> source: " + c.URL + "\n\n" + body
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", title)
	fm.Set("tags", []markdown.Value{"capture"})
	fm.Set("source", c.Source)
	if c.URL != "" {
		fm.Set("url", c.URL)
		// A clipped web page is somebody else's writing. The browser extension
		// is how most of it arrives, and a clipping that looked like the
		// operator's own note would be the widest hole in the trust model —
		// wider than the connectors, because anybody can get a person to clip
		// a page. A capture with no URL is text the person typed or pasted
		// themselves, and stays trusted.
		if host := hostOf(c.URL); host != "" {
			fm.Set("origin", trust.Web(host))
		} else {
			fm.Set("origin", trust.Web(""))
		}
	}
	if _, err := s.Vault.Write(rel, body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// thread into today's daily note so a capture is never orphaned
	drel, dnote, err := s.ensureDaily("")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	stem := rel
	if i := strings.LastIndex(stem, "/"); i >= 0 {
		stem = stem[i+1:]
	}
	stem = strings.TrimSuffix(stem, ".md")
	appended := strings.TrimRight(dnote.Body, " \t\n") +
		fmt.Sprintf("\n- [[%s|%s]]\n", stem, title)
	if _, err := s.Vault.Write(drel, appended, dnote.Frontmatter); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(drel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"path": rel, "title": title})
}

// ---------------------------------------------------------------- settings

func (s *Server) getSettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.settingsState())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var patch map[string]string
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if llm, ok := patch["llm"]; ok {
		switch llm {
		case "", "ollama", "claude":
		default:
			writeErr(w, http.StatusBadRequest, "llm must be '', 'ollama', or 'claude'")
			return
		}
	}
	if err := s.Settings.Update(patch); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.settingsState())
}

func (s *Server) settingsState() map[string]any {
	backend := s.Settings.Get("llm")
	if backend == "" {
		// no configured backend means answers are extractive, not generative
		if s.Settings.Get("ollama_url") != "" {
			backend = "ollama"
		} else {
			backend = "extractive"
		}
	}
	return map[string]any{
		"settings":       s.Settings.AllEffective(),
		"answer_backend": backend,
	}
}
