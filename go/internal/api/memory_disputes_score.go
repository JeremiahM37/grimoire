package api

import (
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/conflict"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Advice for the person settling a dispute.
//
// The listing carries a suggested winner and the score behind it, so the person
// can see which claim the evidence favours and why. The suggestion is advice:
// nothing here resolves anything, and the resolve route is unchanged (a person
// only, and it still writes a human-authority entry).

// disputeSuggestion names the side the scorer favours. Side is "disputed" or
// "challenger", the same words the rest of the listing uses.
type disputeSuggestion struct {
	ID   string `json:"id"`
	Side string `json:"side"`
}

// disputeScore is one side with its breakdown, best first in the listing.
type disputeScore struct {
	ID   string `json:"id"`
	Side string `json:"side"`
	conflict.Breakdown
}

// stampLayout is how appendEntry writes Stamp. A stamp that does not parse
// scores zero recency rather than failing the listing.
const stampLayout = "2006-01-02 15:04"

// adjudicate scores the disputed entry and its challengers together. Each side
// is scored on its own terms, but corroboration is judged within the dispute:
// a side counts as corroborated only by other sides in it that assert the same
// text from an independent origin.
func adjudicate(disputed index.MemoryHit, challengers []index.MemoryHit) (disputeSuggestion, []disputeScore) {
	type side struct {
		hit  index.MemoryHit
		name string
	}
	sides := []side{{disputed, "disputed"}}
	for _, c := range challengers {
		sides = append(sides, side{c, "challenger"})
	}

	// Independence is by origin, or by agent when a fact has none: two facts
	// from one connector are one source, not two.
	origin := func(h index.MemoryHit) string {
		if h.Origin != "" {
			return h.Origin
		}
		return "agent:" + h.Agent
	}
	text := func(h index.MemoryHit) string { return strings.ToLower(strings.Join(strings.Fields(h.Text), " ")) }
	asserts := map[string]map[string]bool{}
	for _, s := range sides {
		t := text(s.hit)
		if asserts[t] == nil {
			asserts[t] = map[string]bool{}
		}
		asserts[t][origin(s.hit)] = true
	}

	now := vault.Now()
	cands := make([]conflict.Candidate, 0, len(sides))
	for i, s := range sides {
		validFrom, _ := time.Parse(stampLayout, s.hit.Stamp)
		cands = append(cands, conflict.Candidate{
			// The candidate id is the side's position, because a fact's own id
			// can repeat across notes (see docs/DISPUTES.md, Limits).
			ID:            strconv.Itoa(i),
			Agent:         s.hit.Agent,
			Authority:     s.hit.Authority(),
			Verification:  s.hit.Verification,
			Corroboration: len(asserts[text(s.hit)]),
			ValidFrom:     validFrom,
		})
	}

	ranked := conflict.Rank(cands, conflict.Options{Now: now})
	scores := make([]disputeScore, 0, len(ranked))
	for _, r := range ranked {
		i, _ := strconv.Atoi(r.Candidate.ID)
		scores = append(scores, disputeScore{
			ID: sides[i].hit.ID, Side: sides[i].name, Breakdown: r.Breakdown,
		})
	}
	top := scores[0]
	return disputeSuggestion{ID: top.ID, Side: top.Side}, scores
}
