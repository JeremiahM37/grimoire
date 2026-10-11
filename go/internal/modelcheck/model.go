// Package modelcheck is a reference model of the memory store's write
// semantics, written from the documented rules (MEMORY_WRITING.md,
// DISPUTES.md, MEMORY-SEAL.md and the authority rules in
// internal/memory/authority.go) rather than from the implementation, plus a
// seeded generator of operation sequences and a shrinker for failing ones.
//
// It has no dependency on the server. The runner that drives the real server
// and compares it with this model lives in internal/api, next to the server
// helpers it needs.
//
// The model deliberately covers a small universe: two slots (subjects that
// share one predicate), three values, three writers at different rungs of
// the authority lattice, and a private visibility flag. It does not model
// negation-retractions, immutable facts, validity windows, sealed facts or
// multi-note behaviour; those are listed as limits in docs/lab/oracle.md.
package modelcheck

// Authority is the rung a writer or an entry sits on. The order is the
// lattice order: a write may supersede an entry at its own rung or below.
type Authority int

const (
	Pulled Authority = iota // connector content other people can write
	Agent                   // an agent asserting something itself
	Human                   // a person; nothing outranks it
)

func (a Authority) String() string {
	switch a {
	case Human:
		return "human"
	case Pulled:
		return "pulled"
	default:
		return "agent"
	}
}

// Writers is the fixed set of writers the generator draws from.
var Writers = []Writer{
	{Name: "me", Auth: Human},
	{Name: "claude", Auth: Agent},
	{Name: "jira", Auth: Pulled, Origin: "connector:jira:OPS-1"},
}

// Writer is who asserts a fact: a person, an agent, or pulled connector text.
type Writer struct {
	Name   string
	Auth   Authority
	Origin string // non-empty only for pulled content
}

// Slots, Values name the universe. A slot is one subject with the predicate
// "runs on port"; a value is a port number.
var (
	Slots  = []string{"Billing Postgres", "Analytics Postgres"}
	Values = []string{"5432", "6432", "6543"}
)

// State of one fact that ever existed.
type State int

const (
	Live       State = iota // believed
	Superseded              // replaced by a later fact (By)
	Retracted               // struck through with attribution (RetractedBy)
	Forgotten               // hard-removed from the file; a tombstone remains
)

func (s State) String() string {
	return [...]string{"live", "superseded", "retracted", "forgotten"}[s]
}

// Fact is one entry in the model's ledger. ID is the ledger ordinal.
type Fact struct {
	ID      int
	Slot    int
	Value   int
	Writer  int
	Auth    Authority // authority of the writer when it was written
	Private bool
	Human   bool // the person stands behind it (written by a person, or kept)
	State   State
	By      int    // successor ID for Superseded, -1 otherwise
	Who     string // for Retracted: "human" or the agent name
	Ch      int    // ID of the fact this one contests, -1 if none
	Created int    // clock step at creation
	Changed int    // clock step of the last state change, -1 if none
	// Challengers are facts that contest this one and are still live; the
	// model derives them from Ch on read, so they need no field.
}

// Text is the fact as the store records it.
func (f Fact) Text() string {
	return Slots[f.Slot] + " runs on port " + Values[f.Value]
}

// Model is the reference store. Clock is the step counter; each operation
// that writes advances it by one, and the runner maps a step to a minute.
type Model struct {
	Facts []*Fact
	Clock int
}

// Decision is what the model says a remember does.
type Decision struct {
	Op     string // ADD, UPDATE, NOOP or REFUSED (an outranked retraction)
	Target int    // ID the decision names, or -1
	Ch     int    // ID challenged, or -1
}

func (m *Model) live(slot int) []*Fact {
	var out []*Fact
	for _, f := range m.Facts {
		if f.Slot == slot && f.State == Live {
			out = append(out, f)
		}
	}
	return out
}

// Remember applies a write. The rules, as documented:
//   - the same value already live in the slot: NOOP, nothing changes;
//   - a live fact in the slot outranks the writer: the write is recorded
//     ALONGSIDE it as a challenge, nothing is superseded;
//   - otherwise every live fact in the slot is at the writer's rung or below,
//     so each is superseded by the new one. A superseded fact is never
//     superseded again;
//   - a replacement inherits privacy from what it replaces, so a restatement
//     cannot publish a private fact.
func (m *Model) Remember(slot, value, writer int, private bool) Decision {
	m.Clock++
	w := Writers[writer]
	live := m.live(slot)
	for _, f := range live {
		if f.Value == value {
			return Decision{Op: "NOOP", Target: f.ID, Ch: -1}
		}
	}
	for _, f := range live {
		if w.Auth < f.Auth {
			nf := m.add(slot, value, writer, private, f.ID)
			return Decision{Op: "ADD", Target: nf.ID, Ch: f.ID}
		}
	}
	if len(live) == 0 {
		nf := m.add(slot, value, writer, private, -1)
		return Decision{Op: "ADD", Target: nf.ID, Ch: -1}
	}
	nf := m.add(slot, value, writer, private, -1)
	for _, f := range live {
		f.State, f.By, f.Changed = Superseded, nf.ID, m.Clock
		if f.Private {
			nf.Private = true
		}
	}
	return Decision{Op: "UPDATE", Target: nf.ID, Ch: -1}
}

func (m *Model) add(slot, value, writer int, private bool, ch int) *Fact {
	w := Writers[writer]
	f := &Fact{
		ID: len(m.Facts), Slot: slot, Value: value, Writer: writer,
		Auth: w.Auth, Private: private, Human: w.Auth == Human,
		State: Live, By: -1, Ch: ch, Created: m.Clock, Changed: -1,
	}
	m.Facts = append(m.Facts, f)
	return f
}

// OutranksWriter reports whether live fact id sits above the writer's rung.
func (m *Model) OutranksWriter(id, writer int) bool {
	f := m.Facts[id]
	return f.State == Live && Writers[writer].Auth < f.Auth
}

// Retractable reports whether writer may retract or forget f. A retraction
// is a write, and the lattice forbids an outranked writer from rewriting an
// entry, so an agent may not soft-forget a person's fact.
func (m *Model) Retractable(writer int, f *Fact) bool {
	return Writers[writer].Auth >= f.Auth
}

// Forget soft-retracts a live fact, attributed to who. A person's forget is
// attributed to "human", as the store does when no agent identity is given.
func (m *Model) Forget(writer, id int) bool {
	m.Clock++
	f := m.Facts[id]
	if f.State != Live || !m.Retractable(writer, f) {
		return false
	}
	f.State, f.Who, f.Changed = Retracted, WhoName(writer), m.Clock
	return true
}

// WhoName is the attribution the store records for a writer's retraction.
func WhoName(writer int) string {
	if Writers[writer].Auth == Human {
		return "human"
	}
	return Writers[writer].Name
}

// HardForget removes a fact from the file; it is tombstoned and gone from
// every read, including as_of. It is available to any fact at the writer's
// rung or below, whatever its state.
func (m *Model) HardForget(writer, id int) bool {
	m.Clock++
	f := m.Facts[id]
	if f.State == Forgotten || !m.Retractable(writer, f) {
		return false
	}
	f.State, f.Changed = Forgotten, m.Clock
	return true
}

// Disputed returns the live challengers of a live fact, which makes it a
// dispute. A dispute exists only while both sides are believed.
func (m *Model) Disputed(id int) []*Fact {
	d := m.Facts[id]
	if d.State != Live {
		return nil
	}
	var out []*Fact
	for _, f := range m.Facts {
		if f.State == Live && f.Ch == id {
			out = append(out, f)
		}
	}
	return out
}

// Resolve settles a dispute. Only a person may (the lattice refuses agents).
// keep: the disputed fact stands and is human-confirmed; challengers are
// retracted by a human. accept: the disputed fact is superseded by the chosen
// challenger, which becomes human-authored; other challengers are retracted.
func (m *Model) Resolve(writer, id int, resolution string, challenger int) bool {
	if Writers[writer].Auth != Human {
		return false
	}
	ch := m.Disputed(id)
	if len(ch) == 0 {
		return false
	}
	m.Clock++
	switch resolution {
	case "keep":
		// A kept fact stands on the person's rung from now on, so an agent's
		// later write cannot supersede it (see Entry.Authority, which reads
		// the human flag).
		d := m.Facts[id]
		d.Human, d.Auth = true, Human
		for _, f := range ch {
			f.State, f.Who, f.Changed = Retracted, "human", m.Clock
		}
	case "accept":
		var pick *Fact
		for _, f := range ch {
			if f.ID == challenger {
				pick = f
			}
		}
		if pick == nil {
			return false
		}
		d := m.Facts[id]
		d.State, d.By, d.Changed = Superseded, pick.ID, m.Clock
		pick.Human, pick.Auth, pick.Ch = true, Human, -1
		for _, f := range ch {
			if f != pick {
				f.State, f.Who, f.Changed = Retracted, "human", m.Clock
			}
		}
	}
	return true
}

// Believed returns the live, non-private facts in a slot: what a default
// recall that includes challenged facts must return.
func (m *Model) Believed(slot int, includePrivate bool) []*Fact {
	var out []*Fact
	for _, f := range m.live(slot) {
		if includePrivate || !f.Private {
			out = append(out, f)
		}
	}
	return out
}

// BelievedAt returns the facts believed at step t: created by then, and not
// yet superseded, retracted or forgotten by then. A fact forgotten at any
// time is excluded from every instant, because forget means gone.
func (m *Model) BelievedAt(slot, t int) []*Fact {
	var out []*Fact
	for _, f := range m.Facts {
		if f.Slot != slot || f.Created > t || f.State == Forgotten {
			continue
		}
		if f.State != Live && f.Changed <= t && f.Changed >= 0 {
			continue
		}
		out = append(out, f)
	}
	return out
}
