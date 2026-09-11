package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func (s *Server) knowledgeExtract(w http.ResponseWriter, r *http.Request) {
	if s.Knowledge == nil {
		writeErr(w, http.StatusServiceUnavailable, "knowledge index unavailable")
		return
	}
	var request struct {
		Paths []string `json:"paths"`
		Force bool     `json:"force"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	if err := decoder.Decode(&request); err != nil {
		writeErr(w, http.StatusBadRequest, "expected JSON {paths, force?}")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeErr(w, http.StatusBadRequest, "expected a single JSON object")
		return
	}
	if len(request.Paths) == 0 || len(request.Paths) > 10 {
		writeErr(w, http.StatusBadRequest, "provide between 1 and 10 source paths")
		return
	}
	for _, path := range request.Paths {
		if strings.TrimSpace(path) == "" {
			writeErr(w, http.StatusBadRequest, "source paths must not be empty")
			return
		}
		if _, err := s.Vault.SafePath(path); err != nil {
			writeErr(w, http.StatusBadRequest, "invalid source path")
			return
		}
	}
	s.configureKnowledge()
	results := s.Knowledge.Extract(request.Paths, request.Force, s.knowledgeVisibility(r, filterFor(r, false)))
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
