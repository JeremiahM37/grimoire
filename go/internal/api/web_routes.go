package api

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/websearch"
)

// The web, for when the vault does not have the answer.
//
// Two routes, matching the two things an agent actually needs: find pages, and
// read one. Both are available to any signed-in caller rather than to
// administrators only — searching the web is not an administrative act — but
// neither is open to anonymous callers on a multi-user instance, because an
// open fetch endpoint is a request-forwarding service for whoever finds it.

func (s *Server) webRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/web/search", s.webSearch)
	mux.HandleFunc("POST /api/web/fetch", s.webFetch)
}

func (s *Server) webSearch(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if s.Web == nil || !s.Web.Available() {
		writeErr(w, http.StatusServiceUnavailable, websearch.ErrNotConfigured.Error())
		return
	}
	n := 5
	if v := r.URL.Query().Get("n"); v != "" {
		if k, err := strconv.Atoi(v); err == nil {
			n = k
		}
	}
	results, err := s.Web.Search(r.Context(), r.URL.Query().Get("q"), n)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if results == nil {
		results = []websearch.Result{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"provider": s.Web.Provider(), "results": results})
}

func (s *Server) webFetch(w http.ResponseWriter, r *http.Request) {
	if !s.requireUser(w, r) {
		return
	}
	if s.Web == nil {
		writeErr(w, http.StatusServiceUnavailable, "web fetching is unavailable")
		return
	}
	var in struct {
		URLs     []string `json:"urls"`
		URL      string   `json:"url"`
		MaxChars int      `json:"max_chars"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	urls := in.URLs
	if in.URL != "" {
		urls = append(urls, in.URL)
	}
	// A bound on the batch: this endpoint dials out on the caller's word, and
	// an unbounded list is a way to make the server do a lot of that at once.
	if len(urls) > 10 {
		urls = urls[:10]
	}
	if len(urls) == 0 {
		writeErr(w, http.StatusBadRequest, "no urls")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pages": s.Web.Fetch(r.Context(), urls, in.MaxChars)})
}

// webContext turns search results into passages the ask path can cite, so a
// question can be answered from the vault AND the web in one pass.
func (s *Server) webContext(r *http.Request, query string, n int) []map[string]any {
	if s.Web == nil || !s.Web.Available() || strings.TrimSpace(query) == "" {
		return nil
	}
	results, err := s.Web.Search(r.Context(), query, n)
	if err != nil || len(results) == 0 {
		return nil
	}
	urls := make([]string, 0, len(results))
	for _, res := range results {
		urls = append(urls, res.URL)
	}
	pages := s.Web.Fetch(r.Context(), urls, 6000)
	out := make([]map[string]any, 0, len(pages))
	for i, p := range pages {
		text := p.Text
		if text == "" {
			// The snippet is better than nothing when a page cannot be read —
			// paywalls, bot walls, PDFs — and it keeps the citation.
			text = results[i].Snippet
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		title := p.Title
		if title == "" {
			title = results[i].Title
		}
		out = append(out, map[string]any{
			"path": results[i].URL, "title": title, "chunk": text, "web": true})
	}
	return out
}
