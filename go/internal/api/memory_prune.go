package api

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Eviction for agent memory.
//
// An agent writes far more than anyone reads back, and the store only ever
// grows. Pruning is the one place a fact is taken away on the store's own
// initiative, so it is held to the narrowest rule in the package: a fact is
// evicted only when every one of its protections is absent. The protections
// are stated in index.PruneCandidates and re-checked against the file before
// each retraction, because the index can lag a write the file already holds.
//
// Eviction is a retraction, not a delete. It goes through retractEntry, the
// same path as `forget`, so the fact is struck through and appears in the
// belief-change digest as retracted by memory-prune. Nothing is removed from
// the file, and the audit trail is the same one a person's own forget leaves.

// pruneAgent is who a prune-driven retraction is attributed to.
const pruneAgent = "memory-prune"

// pruneAfter is how old a fact must be before it can be evicted: a fact written
// within the last quarter has not had a chance to be used yet.
const pruneAfter = 90 * 24 * time.Hour

const (
	pruneDefaultBelow = 2
	pruneDefaultMax   = 50
	pruneMaxCap       = 500
)

type pruneIn struct {
	// Apply false is a dry run, which is the default: the candidates are
	// listed and nothing is written.
	Apply bool `json:"apply"`
	// Below is the highest explicit importance eviction may take (1 to 3).
	Below int `json:"below"`
	// Max caps how many facts one run may retract.
	Max int `json:"max"`
}

// pruneEligible is the file-side check, applied to the parsed entry at the
// moment of retraction. It mirrors the index query; the query finds candidates
// cheaply, this decides whether one still is.
func pruneEligible(e memory.Entry, below int, cutoff time.Time) bool {
	if e.Superseded() || e.Immutable || e.HumanAuthored() || e.Challenges != "" ||
		e.Helpful > 0 || e.Agent == "" {
		return false
	}
	if e.Importance < memory.MinImportance || e.Importance > below {
		return false
	}
	at, ok := parseStampLocal(e.Stamp)
	return ok && at.Before(cutoff)
}

func parseStampLocal(s string) (time.Time, bool) {
	t, err := time.ParseInLocation(memory.StampFormat, s, time.Local)
	return t, err == nil
}

// POST /api/memory/prune
//
// Dry run unless apply is true. Returns the candidates either way, so the dry
// run shows exactly what an apply would retract.
func (s *Server) pruneMemory(w http.ResponseWriter, r *http.Request) {
	in := pruneIn{Below: pruneDefaultBelow, Max: pruneDefaultMax}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if in.Below == 0 {
		in.Below = pruneDefaultBelow
	}
	if in.Max == 0 {
		in.Max = pruneDefaultMax
	}
	if in.Below < memory.MinImportance || in.Below > 3 {
		writeErr(w, http.StatusBadRequest, "below must be 1 to 3: a fact rated 4 or 5 is never evicted")
		return
	}
	if in.Max < 1 || in.Max > pruneMaxCap {
		writeErr(w, http.StatusBadRequest, "max must be 1 to 500")
		return
	}

	cutoff := vault.Now().Add(-pruneAfter)
	found, err := s.Index.PruneCandidates(in.Below, cutoff, in.Max)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Only facts the caller may write are candidates for it: a prune is a
	// write, and the caller's reach is the same as for `forget`.
	var cands []index.MemoryHit
	for _, h := range found {
		if s.canWrite(r, h.Note) {
			cands = append(cands, h)
		}
	}

	rows := make([]map[string]any, 0, len(cands))
	for _, h := range cands {
		rows = append(rows, map[string]any{
			"path": h.Note, "id": h.ID, "text": h.Text,
			"importance": h.Importance, "stamp": h.Stamp, "agent": h.Agent,
		})
	}
	if !in.Apply {
		writeJSON(w, http.StatusOK, map[string]any{
			"apply": false, "below": in.Below, "max": in.Max,
			"candidates": rows, "removed": 0,
		})
		return
	}

	removed := 0
	for _, h := range cands {
		existing, err := s.Vault.Read(h.Note)
		if err != nil {
			continue
		}
		current, found := entryInBody(existing.Body, h.ID)
		if !found || !pruneEligible(current, in.Below, cutoff) {
			// Changed since the query ran: a rating, a vote or a person's
			// edit. Leave it, and say so by not counting it.
			continue
		}
		if err := s.retractEntry(h.Note, h.ID, pruneAgent); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return
		}
		removed++
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"apply": true, "below": in.Below, "max": in.Max,
		"candidates": rows, "removed": removed,
	})
}

// entryInBody finds one entry by id in a note body.
func entryInBody(body, id string) (memory.Entry, bool) {
	for _, e := range memory.Parse(body) {
		if e.ID == id {
			return e, true
		}
	}
	return memory.Entry{}, false
}
