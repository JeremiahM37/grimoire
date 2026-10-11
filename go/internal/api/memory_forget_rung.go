package api

import (
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// forgetRung is the authority rung of whoever is asking to forget a fact.
//
// The lattice (internal/memory/authority.go) says an outranked writer may not
// rewrite or retract an entry above its rung, and a person's entry is never
// altered by an agent-initiated forget (ARCHITECTURE.md). A caller with no
// agent identity, or the command line, is the person. Otherwise the caller is
// an agent, unless it declares connector origin, in which case it is pulled
// text, the lowest rung.
//
// The identity is the body's agent field or the verified or claimed header,
// whichever names one: a request that carries only the header is an agent, not
// a person. A pulled claim can only lower the rung, so it cannot be used to
// reach a person's entry.
func forgetRung(r *http.Request, agent, origin string) memory.Authority {
	who := strings.TrimSpace(agent)
	if who == "" {
		if name, ok := isAgentInitiated(r); ok {
			who = name
		}
	}
	if who == "" || who == "human" || who == "cli" {
		return memory.AuthorityHuman
	}
	return memory.AuthorityOf(strings.TrimSpace(origin), false)
}
