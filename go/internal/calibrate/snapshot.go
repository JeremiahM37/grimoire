package calibrate

import (
	"math"
	"sort"
)

// Fact is one remembered fact as the calibrator sees it: who wrote it, what
// has happened to it since, and how it has been used. A snapshot is a slice of
// these. Every field is derived from the store; nothing here is new state.
type Fact struct {
	Note string
	ID   string
	// Agent is who wrote the fact. Empty means no agent wrote it.
	Agent string
	// Importance is the writer's 1..5 rating. Zero is unrated and ranks as 3.
	Importance int
	// Recalled counts recalls that returned this fact. Used counts recalls
	// that were then reported helpful (the existing `uses` column).
	Recalled int
	Used     int
	// AgeDays is days since the fact was written.
	AgeDays float64

	// Protection inputs. Each of these, if set, takes the fact out of the
	// eviction proposals entirely (see Protected).
	Immutable  bool
	Human      bool // a person asserted it, or it is hand-edited
	Open       bool // an unresolved challenge involves it
	Superseded bool
	Private    bool // visibility is set

	// Events are the correction signals attributed to this fact.
	Events []Event
}

// Snapshot is a point-in-time view of the store.
type Snapshot struct {
	Facts []Fact
}

// Counts sums the correction signals of every fact written by agent.
func (s Snapshot) Counts(agent string) Counts {
	var c Counts
	if agent == "" {
		return c
	}
	for _, f := range s.Facts {
		if f.Agent != agent {
			continue
		}
		for _, ev := range f.Events {
			c.Add(ev)
		}
	}
	return c
}

// AgentReliability is the conservative reliability of agent over this
// snapshot: the lower bound of the posterior, see Reliability.
func (s Snapshot) AgentReliability(agent string) float64 {
	return Reliability(s.Counts(agent))
}

// UseProtect is how many helpful uses protect a fact from eviction whatever
// its age. A fact a person's work has leaned on this often is not a candidate.
const UseProtect = 3

// Protected reports whether a fact is outside eviction proposals altogether,
// and why. The reasons are the ones the eviction path has always respected,
// plus frequent use. A protected fact is never proposed, however old.
func Protected(f Fact) (bool, string) {
	switch {
	case f.Immutable:
		return true, "immutable"
	case f.Human:
		return true, "human-authored"
	case f.Open:
		return true, "open challenge"
	case f.Superseded:
		return true, "already superseded"
	case f.Private:
		return true, "visibility set"
	case f.Importance >= 4:
		return true, "rated important"
	case f.Used >= UseProtect:
		return true, "frequently used"
	}
	return false, ""
}

// RetentionScore says how much a fact is earning its place: higher is worth
// keeping. It is a sum of two log-counts, so the first few uses matter a lot
// and the hundredth matters little, and it is zero for a fact nobody has ever
// recalled or used. Age is deliberately not in the score; ProposalOrder uses it
// as the tie-break among facts with no activity, which is where it belongs.
func RetentionScore(f Fact) float64 {
	return 2*math.Log1p(float64(f.Used)) + math.Log1p(float64(f.Recalled))
}

// ProposalOrder returns the facts eviction may propose, most proposable first:
// lowest RetentionScore, then oldest, then lowest importance. Protected facts
// are dropped, not merely moved to the end. The input is not modified.
func ProposalOrder(facts []Fact) []Fact {
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if p, _ := Protected(f); !p {
			out = append(out, f)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if sa, sb := RetentionScore(a), RetentionScore(b); sa != sb {
			return sa < sb
		}
		if a.AgeDays != b.AgeDays {
			return a.AgeDays > b.AgeDays
		}
		if ia, ib := importanceOf(a), importanceOf(b); ia != ib {
			return ia < ib
		}
		if a.Note != b.Note {
			return a.Note < b.Note
		}
		return a.ID < b.ID
	})
	return out
}

// importanceOf reads a fact's importance with the unrated default applied.
func importanceOf(f Fact) int {
	if f.Importance == 0 {
		return 3
	}
	return f.Importance
}
