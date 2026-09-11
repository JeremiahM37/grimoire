package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/knowledge"
)

// knowledgeVisibility is intentionally applied while the graph is assembled,
// before entity counts and edge evidence are calculated. Post-filtering a
// graph leaks the shape of notes the caller cannot read.
func (s *Server) knowledgeVisibility(r *http.Request, f index.Filter) knowledge.Visibility {
	return func(path, space, acl string, private, untrusted bool) bool {
		if private && !f.IncludePrivate {
			return false
		}
		if untrusted && f.TrustedOnly {
			return false
		}
		return s.canReadNote(r, path, acl)
	}
}

func (s *Server) knowledgeGraph(w http.ResponseWriter, r *http.Request) {
	s.configureKnowledge()
	// Construct this even though graph visibility also uses the captured
	// principal: this keeps the route on the same filter contract as retrieval.
	f := filterFor(r, false)
	depth, limit := parseBound(r.URL.Query().Get("depth"), 2, knowledge.MaxDepth), parseBound(r.URL.Query().Get("limit"), 200, knowledge.MaxNodes)
	includeDocuments := true
	if raw := r.URL.Query().Get("include_documents"); raw != "" {
		includeDocuments = truthy(raw)
	}
	o := knowledge.GraphOptions{Seed: r.URL.Query().Get("seed"), Relation: r.URL.Query().Get("relation"), Q: r.URL.Query().Get("q"), Depth: depth, Limit: limit, IncludeDocuments: includeDocuments, IncludeChunks: truthy(r.URL.Query().Get("include_chunks")), DropNoisy: truthy(r.URL.Query().Get("drop_noisy")), MinDegree: parseBound(r.URL.Query().Get("min_degree"), 0, knowledge.MaxNodes)}
	g, err := s.Knowledge.Snapshot(s.knowledgeVisibility(r, f), o)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, g)
}

func (s *Server) knowledgeQuery(w http.ResponseWriter, r *http.Request) {
	s.configureKnowledge()
	var in struct {
		Question string `json:"question"`
		Limit    int    `json:"limit"`
		Depth    *int   `json:"depth"`
		After    string `json:"after"`
		Before   string `json:"before"`
		Expand   bool   `json:"expand"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil || strings.TrimSpace(in.Question) == "" {
		writeErr(w, http.StatusBadRequest, "question required")
		return
	}
	if err := validKnowledgeDates(in.After, in.Before); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	depth := 2
	if in.Depth != nil {
		depth = *in.Depth
		if depth < 0 {
			writeErr(w, http.StatusBadRequest, "depth must not be negative")
			return
		}
	}
	f := filterFor(r, false)
	result, err := s.Knowledge.Query(in.Question, in.Limit, f, s.knowledgeVisibility(r, f), in.After, in.Before, depth, in.Expand, s.AI)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) configureKnowledge() {
	if s.Knowledge == nil {
		return
	}
	if s.AI == nil || !s.AI.Available() {
		s.Knowledge.SetCompleterVersion("unavailable", nil)
		return
	}
	key := s.AI.Backend()
	if s.Settings != nil {
		key += ":" + s.Settings.Get("llm_model") + ":" + s.Settings.Get("ollama_url") + ":" + s.Settings.Get("llm_base_url")
	}
	s.Knowledge.SetCompleterVersion(key, func(prompt string) (string, error) { return s.AI.Complete(prompt, "") })
}

func validKnowledgeDates(after, before string) error {
	var afterDate, beforeDate time.Time
	var err error
	if after != "" {
		afterDate, err = time.Parse("2006-01-02", after)
		if err != nil {
			return fmt.Errorf("after must be YYYY-MM-DD")
		}
	}
	if before != "" {
		beforeDate, err = time.Parse("2006-01-02", before)
		if err != nil {
			return fmt.Errorf("before must be YYYY-MM-DD")
		}
	}
	if after != "" && before != "" && afterDate.After(beforeDate) {
		return fmt.Errorf("after must not be after before")
	}
	return nil
}

func (s *Server) knowledgeSource(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	if _, err := s.Vault.SafePath(path); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	f := filterFor(r, false)
	e, err := s.Knowledge.Source(path, s.knowledgeVisibility(r, f))
	if err != nil {
		writeErr(w, http.StatusNotFound, "source not found")
		return
	}
	writeJSON(w, http.StatusOK, e)
}
func parseBound(raw string, fallback, max int) int {
	if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
		if n > max {
			return max
		}
		return n
	}
	return fallback
}
