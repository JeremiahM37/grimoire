package ai

import (
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// The model-assisted half of the memory engine.
//
// Both operations here have a deterministic implementation in the memory
// package and use the LLM only to do better. That ordering is deliberate: a
// memory write happens on the hot path of an agent loop, an unreachable model
// must never lose a fact, and a rule that can be unit-tested is worth more
// than a prompt that cannot. Every LLM path below falls back to its rule on
// any error, any unparseable reply, and any answer that names something that
// does not exist.

// ExtractFacts splits a remembered blob into individually-reconcilable facts.
// An LLM separates clauses that share a subject ("prefers tabs and hates
// trailing whitespace") which sentence splitting cannot; without one, the
// deterministic split runs.
func (c *Client) ExtractFacts(text string) []string {
	rule := memory.ExtractFacts(text)
	backend := c.Backend()
	// One short sentence is already one fact; asking a model costs a round
	// trip on the write path to be told so.
	if backend == "" || len(rule) > 1 && len(strings.Fields(text)) < 12 {
		return rule
	}
	if len(strings.Fields(text)) < 8 {
		return rule
	}
	out, err := c.Complete(guidance(c.get("memory_extract_prompt"))+
		"Split the statement into the distinct facts it asserts. "+
		"Each line must stand alone: repeat the subject rather than starting a "+
		"line with a pronoun or a conjunction. Do not add anything that is not "+
		"stated. If it asserts one fact, return it unchanged. "+
		"One fact per line, no numbering, no other text.\n\n"+
		"Statement: "+text+"\n\nFacts:", backend)
	if err != nil {
		return rule
	}
	var facts []string
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(leadingBullet.ReplaceAllString(ln, ""))
		if len(s) > 2 {
			facts = append(facts, s)
		}
		if len(facts) == 12 {
			break
		}
	}
	if len(facts) == 0 {
		return rule
	}
	return facts
}

// DecideMemory works out what a new fact does to the facts already on file:
// ADD it, UPDATE (supersede) one of them, DELETE (retract) one of them, or
// NOOP because it is already recorded.
//
// The rule engine decides without a model. With one, the model decides — but
// only within the same vocabulary, and only against a candidate it was shown:
// a reply naming a candidate index that does not exist, or an operation that
// is not one of the four, falls back rather than inventing an edit. An
// immutable candidate is never offered as a target, so no prompt can talk the
// server into overwriting a fact its owner pinned.
func (c *Client) DecideMemory(fact string, candidates []memory.Entry) memory.Decision {
	rule := memory.Decide(fact, candidates)
	backend := c.Backend()
	if backend == "" || len(candidates) == 0 {
		return rule
	}
	// Only live, mutable candidates may be targeted. Superseding an already
	// superseded fact rewrites history rather than extending it.
	var offered []memory.Entry
	for _, e := range candidates {
		if e.Superseded() || e.Immutable {
			continue
		}
		offered = append(offered, e)
		if len(offered) == 12 {
			break
		}
	}
	if len(offered) == 0 {
		return rule
	}
	listing := make([]string, 0, len(offered))
	for i, e := range offered {
		listing = append(listing, "["+strconv.Itoa(i)+"] "+e.Text)
	}
	out, err := c.Complete(guidance(c.get("memory_decide_prompt"))+
		"You maintain an agent's long-term memory. Decide what the NEW FACT "+
		"does to the EXISTING FACTS.\n\n"+
		"Reply with exactly one line in one of these forms:\n"+
		"ADD — the new fact is not covered by any existing fact\n"+
		"NOOP <n> — existing fact n already says this\n"+
		"UPDATE <n> — the new fact replaces existing fact n (same subject and "+
		"attribute, different value)\n"+
		"DELETE <n> — the new fact retracts existing fact n and asserts no "+
		"replacement\n\n"+
		"Prefer ADD when unsure: keeping two facts is recoverable, discarding "+
		"the wrong one is not. Nothing else in the reply.\n\n"+
		"NEW FACT: "+fact+"\n\nEXISTING FACTS:\n"+strings.Join(listing, "\n")+
		"\n\nDecision:", backend)
	if err != nil {
		return rule
	}
	op, target, ok := parseDecision(out, offered)
	if !ok {
		return rule
	}
	switch op {
	case memory.OpAdd:
		return memory.Decision{Op: memory.OpAdd, Text: fact, Why: "model: not covered on file"}
	case memory.OpNoop:
		return memory.Decision{Op: memory.OpNoop, Target: target.ID,
			Why: "model: already recorded: " + target.Text}
	case memory.OpUpdate:
		return memory.Decision{Op: memory.OpUpdate, Text: fact, Target: target.ID,
			Why: "model: supersedes: " + target.Text}
	case memory.OpDelete:
		return memory.Decision{Op: memory.OpDelete, Target: target.ID,
			Why: "model: retracts: " + target.Text}
	}
	return rule
}

// guidance prefixes an operator's instructions to a prompt.
//
// A prefix rather than a replacement, deliberately: the instructions that
// define the reply FORMAT are what the server parses, and a deployment that
// could overwrite them would produce replies the engine silently falls back
// from — the setting would appear to do nothing. This way an operator can bias
// what gets recorded without being able to break how it is read.
func guidance(custom string) string {
	custom = strings.TrimSpace(custom)
	if custom == "" {
		return ""
	}
	return custom + "\n\n"
}

// parseDecision reads the model's one-line verdict. It reports false for
// anything it does not fully understand, including an operation that needs a
// target and did not name a real one.
func parseDecision(reply string, offered []memory.Entry) (memory.Op, memory.Entry, bool) {
	line := strings.TrimSpace(reply)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	fields := strings.Fields(strings.ToUpper(line))
	if len(fields) == 0 {
		return "", memory.Entry{}, false
	}
	op := memory.Op(strings.Trim(fields[0], ":.,"))
	if op == memory.OpAdd {
		return op, memory.Entry{}, true
	}
	if op != memory.OpNoop && op != memory.OpUpdate && op != memory.OpDelete {
		return "", memory.Entry{}, false
	}
	if len(fields) < 2 {
		return "", memory.Entry{}, false
	}
	n, err := strconv.Atoi(strings.Trim(fields[1], "[]:.,"))
	if err != nil || n < 0 || n >= len(offered) {
		return "", memory.Entry{}, false
	}
	return op, offered[n], true
}
