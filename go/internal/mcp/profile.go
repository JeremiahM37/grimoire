package mcp

import (
	"fmt"
	"sort"
	"strings"
)

// EnvTools selects which tools the mount advertises.
const EnvTools = "GRIMOIRE_MCP_TOOLS"

// CoreTools is the lean profile: the six tools an agent reaches for in
// ordinary work — orient, find, read, remember, recall, and spend a credential.
//
// The full list costs an agent roughly 6k tokens of schema on every request,
// before it reads a single message, and every server a person mounts adds its
// own. Most sessions never touch the knowledge-graph, document-import or
// web tools, so paying for them up front is a tax on the common case. The rest
// stay one environment variable away.
var CoreTools = []string{
	"get_briefing",
	"search_notes",
	"read_note",
	"remember",
	"recall",
	"use_credential",
}

// ParseToolProfile reads GRIMOIRE_MCP_TOOLS.
//
// Empty or "all" advertises everything, which is what every deployment before
// this existed got. "core" is CoreTools. Anything else is a comma-separated
// list of tool names, which may include "core" to extend it:
// "core,ask_notes,get_fact". A nil result means unrestricted.
//
// An unknown name is an error rather than a silent omission: a typo would
// otherwise remove a tool the person meant to keep, and the agent would simply
// never mention it.
func ParseToolProfile(raw string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.EqualFold(raw, "all") {
		return nil, nil
	}
	known := make(map[string]bool)
	for _, t := range Tools() {
		known[t.Name] = true
	}
	allowed := make(map[string]bool)
	var unknown []string
	for _, part := range strings.Split(raw, ",") {
		name := strings.TrimSpace(part)
		switch {
		case name == "":
			continue
		case strings.EqualFold(name, "all"):
			return nil, nil
		case strings.EqualFold(name, "core"):
			for _, c := range CoreTools {
				allowed[c] = true
			}
		case known[name]:
			allowed[name] = true
		default:
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown tool(s) %s", EnvTools, strings.Join(unknown, ", "))
	}
	if len(allowed) == 0 {
		return nil, fmt.Errorf("%s: no tools selected", EnvTools)
	}
	return allowed, nil
}

// advertised is the tool list this mount offers, after the profile.
func (s *Server) advertised(all []tool) []tool {
	if s.Profile == nil {
		return all
	}
	out := make([]tool, 0, len(all))
	for _, t := range all {
		if s.Profile[t.Name] {
			out = append(out, t)
		}
	}
	return out
}

// offers reports whether a tool is callable on this mount. A tool left out of
// the profile is refused rather than quietly served: what tools/list shows is
// the contract, and an agent that guessed a hidden name should get the same
// answer as for a name that does not exist.
func (s *Server) offers(name string) bool {
	return s.Profile == nil || s.Profile[name]
}
