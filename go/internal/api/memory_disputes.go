package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/conflict"
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
	// Verification is whether the evidence resolved when it was written; see
	// memory.Entry.Verification.
	Verification string `json:"verification,omitempty"`
}

// disputeOut is one disputed entry and every fact currently contesting it.
type disputeOut struct {
	ID          string        `json:"id"`
	Path        string        `json:"path"`
	Disputed    disputeSide   `json:"disputed"`
	Challengers []disputeSide `json:"challengers"`
	// Suggestion is the side the evidence favours, and Breakdown is every side
	// scored, best first, with the reason for each term (see conflict). It is
	// advice for the person adjudicating: it never resolves the dispute.
	Suggestion disputeSuggestion `json:"suggestion"`
	Breakdown  []disputeScore    `json:"breakdown"`
}

func sideOf(h index.MemoryHit) disputeSide {
	ev := memory.SplitEvidence(h.Evidence)
	if ev == nil {
		ev = []string{} // an empty list, not null, for a caller iterating it
	}
	return disputeSide{
		ID: h.ID, Path: h.Note, Text: h.Text, Agent: h.Agent, Stamp: h.Stamp,
		Authority: h.Authority().String(), Evidence: ev, Verification: h.Verification,
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
func groupDisputes(hits []index.MemoryHit, rel conflict.ReliabilityFn) []disputeOut {
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
		row.Suggestion, row.Breakdown = adjudicate(d, challengers, rel, byID)
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
	writeJSON(w, http.StatusOK, groupDisputes(hits, s.learnedReliability()))
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
	// The named challenger is the one an accept makes the surviving fact. It
	// is not a filter: a dispute is settled as a whole (DISPUTES.md), so every
	// other challenger is settled too, and none is left believed.
	var chosen *index.MemoryHit
	if want := strings.TrimSpace(in.Challenger); want != "" {
		for i := range challengers {
			if challengers[i].ID == want {
				chosen = &challengers[i]
				break
			}
		}
		if chosen == nil {
			writeErr(w, http.StatusBadRequest, "that entry is not contesting this one")
			return
		}
	} else if res == disputeAccept {
		if len(challengers) > 1 {
			writeErr(w, http.StatusBadRequest, "more than one fact contests this entry; "+
				"name the one to accept with challenger")
			return
		}
		chosen = &challengers[0]
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
	retracted := []string{}
	var entry *memory.Entry
	var plan []settlement
	retract := func(h index.MemoryHit) settlement {
		return settlement{hit: h, mutate: func(e *memory.Entry) {
			e.Challenges = ""
			e.SupersededBy = "retracted:" + disputeResolver
			e.SupersededAt = stamp
		}}
	}
	switch res {
	case disputeKeep:
		// The original stands, and every challenger is retracted: the person
		// has ruled on the dispute, not on one of its contenders.
		stands = orig.ID
		for _, c := range challengers {
			plan = append(plan, retract(c))
			retracted = append(retracted, c.ID)
			superseded = append(superseded, c.ID)
		}
		plan = append(plan, settlement{hit: orig, mutate: func(e *memory.Entry) { e.Human = true }})

	case disputeAccept:
		stands = chosen.ID
		plan = append(plan, settlement{hit: orig, mutate: func(e *memory.Entry) {
			e.SupersededBy = chosen.ID
			e.SupersededAt = stamp
		}})
		superseded = append(superseded, orig.ID)
		plan = append(plan, settlement{hit: *chosen, mutate: func(e *memory.Entry) {
			// The person chose this value, so the surviving fact is theirs.
			e.Challenges = ""
			e.Human = true
		}})
		// Other challengers were contesting the original, which is now
		// replaced, and they are not the value that was accepted.
		for _, c := range challengers {
			if c.ID == chosen.ID {
				continue
			}
			plan = append(plan, retract(c))
			retracted = append(retracted, c.ID)
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
			plan = append(plan, settlement{hit: c, mutate: func(e *memory.Entry) {
				e.Challenges = ""
				e.SupersededBy = fresh.ID
				e.SupersededAt = stamp
			}})
			superseded = append(superseded, c.ID)
		}
	}
	if !s.settleAll(w, plan) {
		return
	}

	out := map[string]any{
		"id": orig.ID, "path": orig.Note, "resolution": res,
		"stands": stands, "superseded": superseded, "retracted": retracted,
	}
	if entry != nil {
		out["entry"] = entryOf(*entry, orig.Note)
	}
	writeJSON(w, http.StatusOK, out)
}

// settlement is one entry's edit within a resolution.
type settlement struct {
	hit    index.MemoryHit
	mutate func(*memory.Entry)
}

// settleAll applies a resolution's edits, reading and writing each note once,
// so the entries a resolution touches in one note change in one write. A
// failure is written as the response and reported as false, so the caller can
// stop. Across notes it is not one transaction: an earlier note may already be
// written when a later one fails (DISPUTES.md).
func (s *Server) settleAll(w http.ResponseWriter, plan []settlement) bool {
	var order []string
	byNote := map[string][]settlement{}
	for _, p := range plan {
		if _, seen := byNote[p.hit.Note]; !seen {
			order = append(order, p.hit.Note)
		}
		byNote[p.hit.Note] = append(byNote[p.hit.Note], p)
	}
	for _, note := range order {
		if err := s.mutateEntries(note, byNote[note]); err != nil {
			writeErr(w, statusForEntryErr(err), entryErrMsg(err))
			return false
		}
	}
	return true
}

// mutateEntries is mutateEntry for several entries in one note: one read, one
// snapshot, one write. Every id in the plan must be present in the note.
func (s *Server) mutateEntries(note string, plan []settlement) error {
	existing, err := s.Vault.Read(note)
	if err != nil {
		return err
	}
	entries := memory.Parse(existing.Body)
	var edited []memory.Entry
	for _, p := range plan {
		found := false
		for _, e := range entries {
			if e.ID != p.hit.ID {
				continue
			}
			found = true
			p.mutate(&e)
			edited = append(edited, e)
		}
		if !found {
			return sql.ErrNoRows
		}
	}
	s.History.Snapshot(note, existing.Body)
	body := memory.Rewrite(existing.Body, edited)
	if _, err := s.Vault.Write(note, body, existing.Frontmatter.Clone()); err != nil {
		return err
	}
	_, err = s.Index.Upsert(note)
	return err
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
