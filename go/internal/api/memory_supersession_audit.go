package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Past supersessions, for review. GET lists the ones the gate now judges
// distinct; POST restores one. Neither depends on the `cascade` flag: the
// listing reads history, and a restore changes nothing unless asked.

// supersessionRow is one past supersession as a person reviews it.
type supersessionRow struct {
	ID         string  `json:"id"`
	Path       string  `json:"path"`
	Old        string  `json:"old"`
	New        string  `json:"new"`
	NewID      string  `json:"new_id"`
	At         string  `json:"at,omitempty"`
	Source     string  `json:"source"`
	PReplace   float64 `json:"p_replace"`
	JudgedDist bool    `json:"judged_distinct"`
}

// GET /api/memory/supersessions
//
// Each past supersession is put to the gate again, as a replacement of the
// old statement by the new one. Rows the gate judges distinct are listed with
// their verdict; a row with source "fallback" had no decision server to ask
// and is counted, not listed as distinct.
func (s *Server) listSupersessions(w http.ResponseWriter, r *http.Request) {
	if _, isAgent := isAgentInitiated(r); isAgent {
		writeErr(w, http.StatusForbidden, "agents may read supersessions only through recall; "+
			"the review list is for a person")
		return
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: filterFor(r, false),
		Limit: 5000, Now: vault.Now(), IncludeSuperseded: true})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[string]index.MemoryHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}
	gate := s.cascadeGate()
	ctx, cancel := context.WithTimeout(r.Context(), cascadeTimeout*4)
	defer cancel()
	rows := []supersessionRow{}
	undecided := 0
	for _, h := range hits {
		if !h.Superseded() || strings.HasPrefix(h.SupersededBy, "retracted:") {
			continue
		}
		newer, ok := byID[h.SupersededBy]
		if !ok || h.Visibility == memory.VisSealed || newer.Visibility == memory.VisSealed {
			continue
		}
		v := gate.Decide(ctx, h.Text, newer.Text)
		if v.Source == "fallback" {
			undecided++
		}
		rows = append(rows, supersessionRow{ID: h.ID, Path: h.Note, Old: h.Text, New: newer.Text,
			NewID: newer.ID, At: h.SupersededAt, Source: v.Source, PReplace: v.P,
			JudgedDist: !v.Replace && v.Source != "fallback"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"supersessions": rows, "undecided": undecided,
		"gate_configured": gate.Laya != nil || gate.Jev != nil})
}

type restoreIn struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// POST /api/memory/supersessions/restore un-supersedes one fact. A person did
// it, so the fact is marked human: from here it resists an agent's overwrite
// the way a person's own entry does. The note is snapshotted first (mutateEntry).
func (s *Server) restoreSupersession(w http.ResponseWriter, r *http.Request) {
	var in restoreIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if _, isAgent := isAgentInitiated(r); isAgent {
		writeErr(w, http.StatusForbidden, "a supersession is restored by a person: agents may not")
		return
	}
	id := strings.TrimSpace(in.ID)
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: filterFor(r, true),
		Note: normPath(in.Path), ID: id, Limit: 1, Now: vault.Now(), IncludeSuperseded: true, IncludePrivate: true})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(hits) != 1 || !hits[0].Superseded() {
		writeErr(w, http.StatusNotFound, "no superseded memory entry with that id")
		return
	}
	note := hits[0].Note
	if err := s.mutateEntry(note, id, func(e *memory.Entry) {
		e.SupersededBy = ""
		e.SupersededAt = ""
		e.Human = true
	}); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	s.forgetPriors()
	writeJSON(w, http.StatusOK, map[string]any{"restored": id, "path": note, "human": true})
}
