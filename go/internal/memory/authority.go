package memory

import (
	"os"
	"strings"
	"sync"
)

// The authority lattice: who is allowed to overwrite whom.
//
// Reconciliation has always had one rung of this. `blocks` refused to let a
// fact from text other people can write supersede a fact from the operator,
// because reconciling is a WRITE and a write is what untrusted text must not be
// able to direct. That rule is correct and it is kept here verbatim — it is
// simply not the only asymmetry in the store.
//
// The other one is authorship. A memory note is a file its owner edits, and the
// README promises exactly that: find a wrong fact, fix it in your editor. But
// supersession compared facts by RECENCY alone, so a person's correction
// survived precisely until the agent wrote on that slot again — at which point
// the correction was struck through and the agent's original value restored.
// Worse than losing it: the strike-through records the PERSON as the superseded
// party, so the history reads as though they were corrected by the agent.
//
// Recency is the wrong default when the writers differ in standing. A newer
// guess by an agent should not silently defeat an older assertion by the person
// the agent works for. So the comparison is a partial order on (authority,
// recency) rather than a total order on time:
//
//	human   — a person: `by=human`, a bullet with no trailer, or a bullet whose
//	          id no longer matches its own content
//	agent   — written through remember/MCP, the ordinary case
//	pulled  — connector content, already trust.Untrusted
//
// A write may supersede an entry at its own rung or below. A write from below
// is not discarded — discarding would lose something the operator might want to
// see — it is recorded ALONGSIDE, where both claims are visible and disagree.
// That is the same shape the untrusted rule already used, and the same shape as
// a just-in-time credential grant: the agent may ask, and asking grants nothing.
type Authority int

const (
	// AuthorityPulled is text other people can write.
	AuthorityPulled Authority = iota
	// AuthorityAgent is an agent asserting something itself.
	AuthorityAgent
	// AuthorityHuman is the operator. Nothing outranks it.
	AuthorityHuman
)

func (a Authority) String() string {
	switch a {
	case AuthorityHuman:
		return "human"
	case AuthorityPulled:
		return "pulled"
	default:
		return "agent"
	}
}

// Authority reports the rung an existing entry sits on.
//
// Declared and inferred authorship are both honoured, and they mean the same
// thing: Human is the `by=human` trailer field, HandWritten is the same claim
// derived from the file when nothing declared it. The inference matters more
// than the declaration in practice, because a person correcting a fact in their
// editor does not stop to add a marker — they change the sentence and save.
func (e Entry) Authority() Authority {
	if authorityOn() && (e.Human || e.HandWritten || e.handEdited()) {
		return AuthorityHuman
	}
	if e.Untrusted() {
		return AuthorityPulled
	}
	return AuthorityAgent
}

// AuthorityOf reports the rung an INCOMING fact writes from.
//
// Origin is the existing signal — a fact carrying a connector origin is pulled,
// whatever asked to record it. `human` is the caller declaring that a person is
// asserting this rather than an agent, which is what the CLI's --human flag and
// the API's "human" field set. A pulled origin wins over a human claim: the
// point of the trust rule is that untrusted text must not be able to talk its
// way up the lattice, and "I am a person" is exactly the sentence such text
// would contain.
func AuthorityOf(origin string, human bool) Authority {
	if (Entry{Origin: origin}).Untrusted() {
		return AuthorityPulled
	}
	if human && authorityOn() {
		return AuthorityHuman
	}
	return AuthorityAgent
}

// authorityOn reports whether the AUTHORSHIP rung is in force.
//
// GRIMOIRE_MEMORY_AUTHORITY=off collapses human and agent onto one rung, which
// restores recency-only supersession exactly as it behaved before the lattice
// existed. It is the control arm for benchmarks/durability — a measurement that
// cannot reproduce the old behaviour on demand is comparing against a memory of
// it — and it is deliberately the ONLY thing the switch touches.
//
// The pulled rung is not affected and cannot be turned off here. That rule is a
// security control: untrusted text must not be able to direct a write, and an
// environment variable that quietly disabled it would be a way to ask for the
// vulnerability back.
var authorityOnce struct {
	sync.Once
	on bool
}

func authorityOn() bool {
	authorityOnce.Do(func() {
		authorityOnce.on = !strings.EqualFold(
			strings.TrimSpace(os.Getenv("GRIMOIRE_MEMORY_AUTHORITY")), "off")
	})
	return authorityOnce.on
}

// outranked reports whether a fact written from `incoming` is forbidden from
// superseding, retracting or otherwise rewriting `existing`.
//
// Equal rungs may supersede each other, which is what keeps ordinary use
// working: an agent corrects its own earlier fact, and a connector re-pulling a
// document that has since changed updates what it said before rather than
// accumulating a contradiction on every sync.
func outranked(incoming Authority, existing Entry) bool {
	return incoming < existing.Authority()
}

// handEdited recomputes the id-versus-content check from the entry's own
// fields.
//
// It is deliberately not a read of the HandWritten flag. That flag is set
// during parsing, and reconciliation does not compare against parsed entries —
// it compares against candidates loaded from the INDEX, which is a different
// construction path that never ran ParseLine. Relying on the flag alone meant
// the rule held in unit tests and did nothing in the running server, which is
// exactly what the first cut of this did.
//
// The evidence survives the round trip because the index stores id, stamp,
// agent and text, which is everything the hash is over.
func (e Entry) handEdited() bool {
	return e.ID != "" && looksMinted(e.ID) && !e.idMatchesContent()
}
