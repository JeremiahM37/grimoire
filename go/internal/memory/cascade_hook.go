package memory

import "github.com/JeremiahM37/grimoire/go/internal/exp"

// The `cascade` experiment: a supersession the rules would make between two
// statements that are contested (see contest.go) is not made by the rules.
// The write path decides it with the gate cascade instead. With the flag off
// deferContested is false and the rules run exactly as before.

// deferContested reports whether a supersession of c by fact must be left to
// the cascade.
func deferContested(fact string, c Entry) bool {
	return exp.On("cascade") && Contested(fact, c.Text)
}

// contestedDecision is what a deferred supersession becomes when no gate is
// consulted: the fact is kept alongside the old one, unmarked. A challenge
// would hide the new fact from default recall (index/memory.go AcceptedOnly),
// so the deterministic fallback keeps both visible.
func contestedDecision(fact string, c Entry) Decision {
	return Decision{Op: OpAdd, Text: fact, Contested: c.ID,
		Why: "contested, kept alongside: " + c.Text}
}
