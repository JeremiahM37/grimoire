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
// A pulled claim can only lower the rung, so it cannot be used to reach a
// person's entry. See forgetCallerIsPerson for who counts as a person.
func forgetRung(r *http.Request, agent, origin string) memory.Authority {
	if forgetCallerIsPerson(r, agent) {
		return memory.AuthorityHuman
	}
	return memory.AuthorityOf(strings.TrimSpace(origin), false)
}

// forgetCallerIsPerson reports whether a forget comes from a person. The
// header is the stronger claim: it is what an MCP server or a hook stamps on
// every request it sends, so a request whose header names an agent is that
// agent whatever its body says. A body that says "human" cannot lift an
// agent's request to a person's. With no agent in the header, the body's agent
// field decides, and an empty one is the person at the console.
func forgetCallerIsPerson(r *http.Request, bodyAgent string) bool {
	if _, ok := isAgentInitiated(r); ok {
		return false
	}
	who := strings.TrimSpace(bodyAgent)
	return who == "" || who == "human" || who == "cli"
}
