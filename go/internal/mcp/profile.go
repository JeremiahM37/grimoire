package mcp

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// EnvTools selects which tools the mount advertises.
const EnvTools = "GRIMOIRE_MCP_TOOLS"

// CoreTools is the default profile: the tools an agent reaches for in ordinary
// work — orient, find, read, write a note, remember, recall, save a procedure,
// and spend a credential. A procedure is memory, so it sits here too. Asking a
// person for a credential is in the credentials group: across a month of real
// transcripts no agent called it, and it costs about 2 KB of schema.
//
// Every advertised schema is paid for on every request, before the agent reads
// a message, and every server a person mounts adds its own. Most sessions never
// touch the knowledge graph, the memory banks or the web tools, so those are
// grouped (ToolGroups) and listed only when the operator asks for them.
var CoreTools = []string{
	"get_briefing",
	"search_notes",
	"read_note",
	"ask_notes",
	"create_note",
	"update_note",
	"append_daily",
	"remember",
	"recall",
	"get_fact",
	"remember_procedure",
	"use_credential",
}

// toolGroup is one named, non-core set of tools. Every advertised tool is in
// CoreTools or in exactly one group; a test fails when a new tool is in neither,
// so a tool cannot be silently orphaned from GRIMOIRE_MCP_TOOLS.
type toolGroup struct {
	name  string
	tools []string
}

var toolGroups = []toolGroup{
	{"notes", []string{"list_notes", "backlinks", "list_tags", "stale_notes", "open_urls", "search_web", "kb_info"}},
	{"memory", []string{"remember_image", "forget", "memory_changes", "memory_profile", "memory_prefix",
		"memory_disputes", "memory_resolve_dispute", "memory_graph", "memory_feedback", "memory_scopes", "set_fact"}},
	{"banks", []string{"retain", "bank_recall", "reflect", "bank_index", "bank_timeline", "bank_duplicates",
		"bank_merge_duplicates", "bank_get", "list_banks", "create_bank", "bank_profile", "list_bank_memories",
		"get_bank_memory", "delete_bank_memory", "list_entities", "list_bank_documents", "get_bank_document",
		"delete_bank_document", "consolidate", "list_observations", "update_observation", "list_mental_models",
		"get_mental_model", "create_mental_model", "update_mental_model", "delete_mental_model",
		"refresh_mental_model", "list_directives", "create_directive", "delete_directive",
		"list_bank_templates", "import_bank_template"}},
	{"knowledge", []string{"query_knowledge", "knowledge_graph", "read_source", "extract_relationships",
		"list_documents", "refresh_document", "import_document", "sources", "source_search", "source_read",
		"source_act", "source_action_status"}},
	{"credentials", []string{"list_grants", "request_credential", "check_credential_request"}},
	{"code", []string{"code_symbol", "code_callers", "code_outline"}},
	{"admin", []string{"consolidate_memory", "dream", "list_operations", "get_operation", "cancel_operation"}},
}

// GroupNames lists the named groups in the order they are documented.
func GroupNames() []string {
	out := make([]string, len(toolGroups))
	for i, g := range toolGroups {
		out[i] = g.name
	}
	return out
}

// GroupTools returns the tools in a named group, or nil for an unknown name.
func GroupTools(name string) []string {
	for _, g := range toolGroups {
		if g.name == name {
			return g.tools
		}
	}
	return nil
}

// knownTools is every tool the server implements, built once.
var knownTools = sync.OnceValue(func() map[string]bool {
	known := make(map[string]bool)
	for _, t := range Tools() {
		known[t.Name] = true
	}
	return known
})

// ParseToolProfile reads GRIMOIRE_MCP_TOOLS: the set of tools that tools/list
// advertises.
//
// Unset or empty is "core". "all" advertises everything, which is what every
// deployment before profiles got. Otherwise it is a comma-separated list of
// group names (core, notes, memory, banks, knowledge, credentials, code,
// admin), tool names, or both: "core,banks" or "core,ask_notes,get_fact". A
// nil result means unrestricted.
//
// Only what is advertised changes: a tool left out of the profile is still
// callable by name, so a client or prompt that already knows it keeps working.
//
// An unknown name is an error rather than a silent omission: a typo would
// otherwise remove a tool the person meant to keep, and the agent would simply
// never mention it.
func ParseToolProfile(raw string) (map[string]bool, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "core"
	}
	if strings.EqualFold(raw, "all") {
		return nil, nil
	}
	known := knownTools()
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
		case GroupTools(name) != nil:
			for _, t := range GroupTools(name) {
				allowed[t] = true
			}
		case known[name]:
			allowed[name] = true
		default:
			unknown = append(unknown, name)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown tool or group %s (groups: core, %s, all)",
			EnvTools, strings.Join(unknown, ", "), strings.Join(GroupNames(), ", "))
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

// offers reports whether a tool can be called on this mount. The profile only
// decides what tools/list shows; a tool that is hidden but real still answers,
// so an agent or prompt that names it keeps working. A name that is not a tool
// at all is refused as before.
func (s *Server) offers(name string) bool {
	return knownTools()[name]
}
