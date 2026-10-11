package api

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Promotion clears the taint on untrusted bank content. It is the one way out
// of the untrusted state, so it is admin-gated, and it refuses any request that
// names an agent. An agent that omits the header can still reach it with an
// admin token; the header is a claim, not an identity.

type promoteIn struct {
	Reason string `json:"reason"`
}

func decodePromote(r *http.Request) (string, bool) {
	var in promoteIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return "", false
	}
	return strings.TrimSpace(in.Reason), true
}

func (s *Server) promoteUnit(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	reason, ok := decodePromote(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	unit := r.PathValue("id")
	if err := s.Banks.PromoteUnit(id, unit, reason, agentFor(r) == ""); err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"promoted": unit, "bank_id": id})
}

func (s *Server) promoteModel(w http.ResponseWriter, r *http.Request) {
	id, ok := s.bankWritable(w, r)
	if !ok {
		return
	}
	reason, ok := decodePromote(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	m, err := s.Banks.PromoteModel(id, r.PathValue("id"), reason, agentFor(r) == "")
	if err != nil {
		writeBankErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}
