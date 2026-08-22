package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Open disagreements between an agent and the person it works for.
//
// The authority lattice stops an agent's write from reverting a correction, but
// refusing an overwrite is only half an answer. The agent still believes
// something, it still had a reason, and the person may well be the one who is
// out of date — they wrote their fact before the deploy moved. Leaving the two
// claims side by side with nothing joining them is what the immutable flag
// already did, and it reads as clutter rather than as a question.
//
// So a refused supersession records what it contested, and this is where those
// come back: a list of "your agent thinks this is wrong, here is what it thinks
// instead", and one call to settle it either way. Upholding retracts the
// agent's claim; conceding lets it supersede after all, which is the person
// deciding rather than the agent deciding for them.
//
// It is the same shape as a just-in-time credential grant, which is not a
// coincidence — it is the same problem. The agent may ask, asking grants
// nothing, and a person answers.

type challengeOut struct {
	// Challenger is the agent's contested claim.
	ID    string `json:"id"`
	Text  string `json:"text"`
	Agent string `json:"agent,omitempty"`
	Stamp string `json:"stamp,omitempty"`
	Note  string `json:"note"`

	// Contested is the fact it disagrees with, and could not overwrite.
	ContestedID        string `json:"contested_id"`
	ContestedText      string `json:"contested_text"`
	ContestedAuthority string `json:"contested_authority"`
	ContestedStamp     string `json:"contested_stamp,omitempty"`
}

// challenges lists disagreements nobody has settled.
//
// A challenge whose contested fact is gone — retracted, expired, superseded by
// the person themselves — is not listed. The question it asked has been
// answered by other means, and showing it would be asking a person to rule on
// something that no longer exists.
func (s *Server) challenges(w http.ResponseWriter, r *http.Request) {
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, true),
		Limit:  challengeListLimit,
		Now:    vault.Now(),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[string]index.MemoryHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}
	now := vault.Now()
	out := make([]challengeOut, 0)
	for _, h := range hits {
		if h.Challenges == "" || !h.Live(now) {
			continue
		}
		contested, ok := byID[h.Challenges]
		if !ok || !contested.Live(now) {
			continue
		}
		out = append(out, challengeOut{
			ID: h.ID, Text: h.Text, Agent: h.Agent, Stamp: h.Stamp, Note: h.Note,
			ContestedID: contested.ID, ContestedText: contested.Text,
			ContestedAuthority: contested.Authority().String(),
			ContestedStamp:     contested.Stamp,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// challengeListLimit bounds the scan. Challenges are rare by construction —
// each one is an agent contradicting its operator — so a cap this high is a
// backstop rather than a page size.
const challengeListLimit = 2000

type challengeIn struct {
	Note string `json:"note"`
	ID   string `json:"id"`

	// Resolution is "uphold" — the person's fact stands and the agent's claim
	// is retracted — or "concede", which lets the agent's claim supersede after
	// all. Conceding is how a person corrects THEMSELVES, and it has to be
	// available or the lattice would make the operator's first answer permanent.
	Resolution string `json:"resolution"`
}

// resolveChallenge settles one disagreement.
func (s *Server) resolveChallenge(w http.ResponseWriter, r *http.Request) {
	var in challengeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	note, id := strings.TrimSpace(in.Note), strings.TrimSpace(in.ID)
	if note == "" || id == "" {
		writeErr(w, http.StatusBadRequest, "note and id are required")
		return
	}
	switch strings.ToLower(strings.TrimSpace(in.Resolution)) {
	case "uphold":
		s.settleChallenge(w, note, id, false)
	case "concede":
		s.settleChallenge(w, note, id, true)
	default:
		writeErr(w, http.StatusBadRequest, `resolution must be "uphold" or "concede"`)
	}
}

// settleChallenge applies the person's ruling.
//
// Both outcomes are recorded as supersession rather than deletion, so the note
// keeps the whole exchange: what the agent claimed, what it contested, and
// which way it went. A challenge that vanished when it was settled would make
// the audit trail worse than not having challenges at all.
func (s *Server) settleChallenge(w http.ResponseWriter, note, id string, concede bool) {
	var contestedID string
	if err := s.mutateEntry(note, id, func(e *memory.Entry) {
		contestedID = e.Challenges
	}); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if contestedID == "" {
		writeErr(w, http.StatusBadRequest, "that fact is not challenging anything")
		return
	}

	loser, winner := id, contestedID
	if concede {
		loser, winner = contestedID, id
	}
	stamp := vault.Now().Format(memory.StampFormat)
	// The losing side is struck through and points at what replaced it, which
	// is exactly what an ordinary supersession writes — a reader of the file
	// does not need to know a challenge was involved to understand it.
	if err := s.mutateEntry(note, loser, func(e *memory.Entry) {
		e.SupersededBy = winner
		e.SupersededAt = stamp
	}); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	// The challenge is cleared either way: it has been answered, and leaving it
	// set would keep asking.
	if err := s.mutateEntry(note, id, func(e *memory.Entry) {
		e.Challenges = ""
		if concede {
			// The person accepted the agent's value, so the surviving fact is
			// now theirs to stand behind.
			e.Human = true
		}
	}); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"resolved": id, "superseded": loser, "stands": winner,
		"resolution": map[bool]string{true: "concede", false: "uphold"}[concede],
	})
}
