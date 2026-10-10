package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Dispute adjudication.
//
// reconcile.go refuses to let a lower-authority write supersede a higher one
// and records the refusal as a challenge: the contested entry is the one that
// outranks the writer, and the challenger is the new fact, parked beside it.
// challenges.go lets a person uphold or concede. This is the fuller version of
// the same act, for a person who needs to settle a disagreement on their own
// terms:
//
//	keep             the original stands; the challenger is retracted, and
//	                 the original is marked human-confirmed
//	accept_challenger the challenger supersedes the original and becomes
//	                 human-authored; any other challenger is retracted
//	merge            new human-authored text supersedes the original and every
//	                 challenger at once
//
// The principle is that a human edit always wins and a resolution always
// writes a human-authority entry, so resolution is a person's act. An agent
// may read the disputes (they carry text the caller is already allowed to
// see) but may not settle them — not even against an agent-rung entry, because
// the outcome would be a human-authority write it was never entitled to make.
// That is stricter than the authority lattice requires, deliberately: the
// lattice says what may overwrite what, and this is the one place a write is
// labelled as a person's.
//
// Every resolution is a belief change, so it appears in /api/memory/changes
// without any new storage: a retraction is a `retracted` row attributed to the
// resolver, and a supersession is a `changed` row carrying both texts.

// disputeResolver is the name a resolution is attributed to. A dispute is
// settled by a person, and the same name is what a forget without an agent
// header is attributed to.
const disputeResolver = "human"

// The resolutions a dispute accepts.
const (
	disputeKeep    = "keep"
	disputeAccept  = "accept_challenger"
	disputeMerge   = "merge"
	disputeMaxText = 20000
)

// disputeSide is one side of a disagreement: a fact as it stands, with the
// evidence it was written from and the rung it sits on.
type disputeSide struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	Text      string   `json:"text"`
	Agent     string   `json:"agent,omitempty"`
	Stamp     string   `json:"stamp,omitempty"`
	Authority string   `json:"authority"`
	Evidence  []string `json:"evidence"`
}

// disputeOut is one disputed entry and every fact currently contesting it.
type disputeOut struct {
	ID          string        `json:"id"`
	Path        string        `json:"path"`
	Disputed    disputeSide   `json:"disputed"`
	Challengers []disputeSide `json:"challengers"`
}

func sideOf(h index.MemoryHit) disputeSide {
	ev := memory.SplitEvidence(h.Evidence)
	if ev == nil {
		ev = []string{} // an empty list, not null, for a caller iterating it
	}
	return disputeSide{
		ID: h.ID, Path: h.Note, Text: h.Text, Agent: h.Agent, Stamp: h.Stamp,
		Authority: h.Authority().String(), Evidence: ev,
	}
}

// liveDisputeHits returns the entries the caller may see and that are still
// believed, which is the only population a dispute can be drawn from.
func (s *Server) liveDisputeHits(r *http.Request) ([]index.MemoryHit, error) {
	return s.Index.MemoryEntries(index.MemoryQuery{
		Filter: filterFor(r, true),
		Limit:  challengeListLimit,
		Now:    vault.Now(),
	})
}

// groupDisputes pairs each contested entry with the live challengers pointing
// at it. A challenge whose contested entry is no longer believed is not a
// dispute any more, and is left out for the same reason challenges.go leaves
// it out: asking a person to rule on something that no longer stands is asking
// about nothing.
func groupDisputes(hits []index.MemoryHit) []disputeOut {
	now := vault.Now()
	byID := make(map[string]index.MemoryHit, len(hits))
	for _, h := range hits {
		byID[h.ID] = h
	}
	groups := map[string][]index.MemoryHit{}
	for _, h := range hits {
		if h.Challenges == "" || !h.Live(now) {
			continue
		}
		if contested, ok := byID[h.Challenges]; !ok || !contested.Live(now) {
			continue
		}
		groups[h.Challenges] = append(groups[h.Challenges], h)
	}
	out := make([]disputeOut, 0, len(groups))
	for id, challengers := range groups {
		d := byID[id]
		sort.SliceStable(challengers, func(i, j int) bool {
			if challengers[i].Stamp != challengers[j].Stamp {
				return challengers[i].Stamp < challengers[j].Stamp
			}
			return challengers[i].ID < challengers[j].ID
		})
		row := disputeOut{ID: d.ID, Path: d.Note, Disputed: sideOf(d), Challengers: []disputeSide{}}
		for _, c := range challengers {
			row.Challengers = append(row.Challengers, sideOf(c))
		}
		out = append(out, row)
	}
	// Newest disagreement first, so a person reads what most recently needs
	// them at the top.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Disputed.Stamp != out[j].Disputed.Stamp {
			return out[i].Disputed.Stamp > out[j].Disputed.Stamp
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GET /api/memory/disputes lists the disagreements nobody has settled.
func (s *Server) listDisputes(w http.ResponseWriter, r *http.Request) {
	hits, err := s.liveDisputeHits(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, groupDisputes(hits))
}

type disputeIn struct {
	ID         string `json:"id"`
	Path       string `json:"path"`
	Resolution string `json:"resolution"`
	Text       string `json:"text"`
	Challenger string `json:"challenger"`
}

// POST /api/memory/disputes/resolve settles one dispute.
func (s *Server) resolveDispute(w http.ResponseWriter, r *http.Request) {
	var in disputeIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	id := strings.TrimSpace(in.ID)
	res := strings.ToLower(strings.TrimSpace(in.Resolution))
	if id == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	switch res {
	case disputeKeep, disputeAccept, disputeMerge:
	default:
		writeErr(w, http.StatusBadRequest, `resolution must be "keep", "accept_challenger" or "merge"`)
		return
	}
	text := strings.TrimSpace(in.Text)
	if res == disputeMerge && (text == "" || len(text) > disputeMaxText) {
		writeErr(w, http.StatusBadRequest, "merge needs text of 1 to 20000 characters")
		return
	}
	// Checked before anything is read, so an agent gets the same answer
	// whether or not the entry exists: no existence oracle for agents.
	if _, isAgent := isAgentInitiated(r); isAgent {
		writeErr(w, http.StatusForbidden, "a dispute is settled by a person: agents may "+
			"read the disputes, not resolve them")
		return
	}

	hits, err := s.liveDisputeHits(r)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	path := normPath(in.Path)
	var matches []index.MemoryHit
	for _, h := range hits {
		if h.ID == id && (path == "" || h.Note == path) {
			matches = append(matches, h)
		}
	}
	if len(matches) == 0 {
		writeErr(w, http.StatusNotFound, "no such disputed memory entry")
		return
	}
	if len(matches) > 1 {
		writeErr(w, http.StatusConflict, "more than one entry has that id; pass path")
		return
	}
	orig := matches[0]

	var challengers []index.MemoryHit
	for _, h := range hits {
		if h.Challenges == orig.ID {
			challengers = append(challengers, h)
		}
	}
	if len(challengers) == 0 {
		writeErr(w, http.StatusConflict, "that entry is not disputed")
		return
	}
	if want := strings.TrimSpace(in.Challenger); want != "" {
		picked := challengers[:0:0]
		for _, c := range challengers {
			if c.ID == want {
				picked = append(picked, c)
			}
		}
		if len(picked) == 0 {
			writeErr(w, http.StatusBadRequest, "that entry is not contesting this one")
			return
		}
		challengers = picked
	} else if res == disputeAccept && len(challengers) > 1 {
		writeErr(w, http.StatusBadRequest, "more than one fact contests this entry; "+
			"name the one to accept with challenger")
		return
	}
	if res == disputeAccept && len(challengers) != 1 {
		writeErr(w, http.StatusBadRequest, "accept_challenger needs exactly one challenger")
		return
	}

	// Every note the resolution touches must be writable, checked before the
	// first write so a refusal cannot leave the dispute half-settled.
	notes := []string{orig.Note}
	for _, c := range challengers {
		if c.Note != orig.Note {
			notes = append(notes, c.Note)
		}
	}
	for _, n := range notes {
		if !s.requireWrite(w, r, n) {
			return
		}
	}

	stamp := vault.Now().Format(memory.StampFormat)
	var stands string
	var superseded []string
	var entry *memory.Entry
	switch res {
	case disputeKeep:
		stands = orig.ID
		for _, c := range challengers {
			if !s.settleOne(w, c, func(e *memory.Entry) {
				e.Challenges = ""
				e.SupersededBy = "retracted:" + disputeResolver
				e.SupersededAt = stamp
			}) {
				return
			}
			superseded = append(superseded, c.ID)
		}
		if !s.settleOne(w, orig, func(e *memory.Entry) { e.Human = true }) {
			return
		}

	case disputeAccept:
		chosen := challengers[0]
		stands = chosen.ID
		if !s.settleOne(w, orig, func(e *memory.Entry) {
			e.SupersededBy = chosen.ID
			e.SupersededAt = stamp
		}) {
			return
		}
		superseded = append(superseded, orig.ID)
		if !s.settleOne(w, chosen, func(e *memory.Entry) {
			// The person chose this value, so the surviving fact is theirs.
			e.Challenges = ""
			e.Human = true
		}) {
			return
		}
		// Other challengers were contesting the original, which is now
		// replaced, and they are not the value that was accepted.
		for _, c := range challengers[1:] {
			if !s.settleOne(w, c, func(e *memory.Entry) {
				e.Challenges = ""
				e.SupersededBy = "retracted:" + disputeResolver
				e.SupersededAt = stamp
			}) {
				return
			}
			superseded = append(superseded, c.ID)
		}

	case disputeMerge:
		fresh := memory.Entry{
			Text: text, Agent: disputeResolver, Stamp: stamp, Human: true,
			ID: memory.DeriveID(stamp, disputeResolver, text),
		}
		if err := s.appendDisputeEntry(orig.Note, &fresh); err != nil {
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return
		}
		entry = &fresh
		stands = fresh.ID
		for _, c := range append([]index.MemoryHit{orig}, challengers...) {
			if !s.settleOne(w, c, func(e *memory.Entry) {
				e.Challenges = ""
				e.SupersededBy = fresh.ID
				e.SupersededAt = stamp
			}) {
				return
			}
			superseded = append(superseded, c.ID)
		}
	}

	out := map[string]any{
		"id": orig.ID, "path": orig.Note, "resolution": res,
		"stands": stands, "superseded": superseded,
	}
	if entry != nil {
		out["entry"] = entryOf(*entry, orig.Note)
	}
	writeJSON(w, http.StatusOK, out)
}

// settleOne edits one entry in the note it lives in. A failure is written as
// the response and reported as false, so the caller can stop.
func (s *Server) settleOne(w http.ResponseWriter, h index.MemoryHit, mutate func(*memory.Entry)) bool {
	if err := s.mutateEntry(h.Note, h.ID, mutate); err != nil {
		writeErr(w, statusForEntryErr(err), entryErrMsg(err))
		return false
	}
	return true
}

// appendDisputeEntry writes a new human-authored bullet at the end of a note,
// the same way a remember does, and gives it an id that does not collide.
func (s *Server) appendDisputeEntry(note string, e *memory.Entry) error {
	existing, err := s.Vault.Read(note)
	if err != nil {
		return err
	}
	e.ID = uniqueID(memory.Parse(existing.Body), e.ID)
	s.History.Snapshot(note, existing.Body)
	body := memory.Append(existing.Body, *e)
	if _, err := s.Vault.Write(note, body, existing.Frontmatter.Clone()); err != nil {
		return err
	}
	_, err = s.Index.Upsert(note)
	return err
}
