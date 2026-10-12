package api

import (
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// History: a fact marked as an event is something that happened and stays true
// as history. Behind the experiment flag "events", such a fact is not offered
// to automatic injection (the hook and context path), where a past event read
// as a current state misleads an agent. It stays in explicit recall, search,
// lists and briefings, and explicit recall says it is an event. With the flag
// off, marking a fact changes nothing about what is injected.

const eventsFlag = "events"

// injectable reports whether a fact may be offered to automatic injection.
func injectable(h index.MemoryHit) bool {
	return !(h.Event && exp.On(eventsFlag))
}
