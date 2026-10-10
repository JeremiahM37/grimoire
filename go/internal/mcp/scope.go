package mcp

import "github.com/JeremiahM37/grimoire/go/internal/oauth"

// toolScope maps every advertised tool to the OAuth scope a web-connector
// session needs to call it. Only checked for a request that carries OAuth
// scopes at all (internal/mcp/http.go) — stdio and a static
// GRIMOIRE_MCP_TOKEN caller are the existing local-trust surfaces and stay
// unrestricted, exactly as they were before this feature existed.
//
// Table-driven and checked by TestEveryToolHasAScope, the same shape
// tools_test.go already uses for `behaviour`: a tool that ships unclassified
// here would otherwise be silently unreachable over OAuth (present in
// Tools() but missing from every filtered tools/list), which reads as a bug
// report nobody could reproduce without knowing to look here.
var toolScope = map[string]string{
	// Reads: notes, documents, the knowledge graph, and the web tools that
	// only fetch. None of these can change what's in the vault.
	"kb_info":         oauth.ScopeNotesRead,
	"get_briefing":    oauth.ScopeNotesRead,
	"search_notes":    oauth.ScopeNotesRead,
	"ask_notes":       oauth.ScopeNotesRead,
	"read_note":       oauth.ScopeNotesRead,
	"list_notes":      oauth.ScopeNotesRead,
	"backlinks":       oauth.ScopeNotesRead,
	"list_tags":       oauth.ScopeNotesRead,
	"get_fact":        oauth.ScopeNotesRead,
	"list_documents":  oauth.ScopeNotesRead,
	"read_source":     oauth.ScopeNotesRead,
	"knowledge_graph": oauth.ScopeNotesRead,
	"query_knowledge": oauth.ScopeNotesRead,
	"stale_notes":     oauth.ScopeNotesRead,
	"search_web":      oauth.ScopeNotesRead,
	"open_urls":       oauth.ScopeNotesRead,

	// Writes: create, edit, or pull new material into the vault.
	"create_note":           oauth.ScopeNotesWrite,
	"update_note":           oauth.ScopeNotesWrite,
	"append_daily":          oauth.ScopeNotesWrite,
	"set_fact":              oauth.ScopeNotesWrite,
	"refresh_document":      oauth.ScopeNotesWrite,
	"import_document":       oauth.ScopeNotesWrite,
	"extract_relationships": oauth.ScopeNotesWrite,

	// Agent memory: its own surface, distinct from notes even though it is
	// implemented as notes underneath — see DESIGN.md's Grant/Secret
	// glossary for why memory gets its own scope rather than folding into
	// notes:write.
	"remember":           oauth.ScopeMemory,
	"recall":             oauth.ScopeMemory,
	"memory_changes":     oauth.ScopeMemory,
	"memory_profile":     oauth.ScopeMemory,
	"memory_graph":       oauth.ScopeMemory,
	"memory_feedback":    oauth.ScopeMemory,
	"memory_scopes":      oauth.ScopeMemory,
	"forget":             oauth.ScopeMemory,
	"consolidate_memory": oauth.ScopeMemory,
	"dream":              oauth.ScopeMemory,

	// The code graph reads source repositories. It is admin-only at the API;
	// the scope is the read scope, since it is not a write and not a memory.
	"code_symbol":  oauth.ScopeNotesRead,
	"code_callers": oauth.ScopeNotesRead,
	"code_outline": oauth.ScopeNotesRead,

	// Memory banks are agent memory too.
	"retain":                oauth.ScopeMemory,
	"bank_recall":           oauth.ScopeMemory,
	"bank_index":            oauth.ScopeMemory,
	"bank_duplicates":       oauth.ScopeMemory,
	"bank_merge_duplicates": oauth.ScopeMemory,
	"bank_timeline":         oauth.ScopeMemory,
	"bank_get":              oauth.ScopeMemory,
	"list_banks":            oauth.ScopeMemory,
	"create_bank":           oauth.ScopeMemory,
	"bank_profile":          oauth.ScopeMemory,
	"list_bank_memories":    oauth.ScopeMemory,
	"delete_bank_memory":    oauth.ScopeMemory,
	"list_entities":         oauth.ScopeMemory,
	"list_bank_documents":   oauth.ScopeMemory,
	"get_bank_document":     oauth.ScopeMemory,
	"delete_bank_document":  oauth.ScopeMemory,
	"reflect":               oauth.ScopeMemory,
	"consolidate":           oauth.ScopeMemory,
	"list_observations":     oauth.ScopeMemory,
	"get_bank_memory":       oauth.ScopeMemory,
	"list_mental_models":    oauth.ScopeMemory,
	"get_mental_model":      oauth.ScopeMemory,
	"create_mental_model":   oauth.ScopeMemory,
	"update_observation":    oauth.ScopeMemory,
	"update_mental_model":   oauth.ScopeMemory,
	"delete_mental_model":   oauth.ScopeMemory,
	"refresh_mental_model":  oauth.ScopeMemory,
	"list_directives":       oauth.ScopeMemory,
	"create_directive":      oauth.ScopeMemory,
	"delete_directive":      oauth.ScopeMemory,
	"list_operations":       oauth.ScopeMemory,
	"get_operation":         oauth.ScopeMemory,
	"cancel_operation":      oauth.ScopeMemory,
	"list_bank_templates":   oauth.ScopeMemory,
	"import_bank_template":  oauth.ScopeMemory,

	// The credential broker. Kept apart from everything else — see
	// oauth.DefaultScopes — because this is the one scope that can spend a
	// credential against a third party on the owner's behalf.
	"list_grants":              oauth.ScopeCredentials,
	"use_credential":           oauth.ScopeCredentials,
	"request_credential":       oauth.ScopeCredentials,
	"check_credential_request": oauth.ScopeCredentials,

	// Connected sources use the owner's third-party credentials on the
	// owner's behalf, so they ride the credentials scope, which a web
	// connector does not get by default. Listing them reveals only names.
	"sources":              oauth.ScopeNotesRead,
	"source_search":        oauth.ScopeCredentials,
	"source_read":          oauth.ScopeCredentials,
	"source_act":           oauth.ScopeCredentials,
	"source_action_status": oauth.ScopeCredentials,
}

// scopeFor reports the scope a tool needs, and whether it is classified at
// all — every entry in Tools() must be.
func scopeFor(name string) (string, bool) {
	s, ok := toolScope[name]
	return s, ok
}

// filterToolsByScope keeps only the tools granted covers. Used for
// tools/list on a scoped (OAuth) session; nil granted means unrestricted and
// is never passed through this function — see http.go.
func filterToolsByScope(all []tool, granted []string) []tool {
	out := make([]tool, 0, len(all))
	for _, t := range all {
		need, ok := scopeFor(t.Name)
		if !ok || scopeGrantedLocal(granted, need) {
			out = append(out, t)
		}
	}
	return out
}

// scopeGrantedLocal avoids importing oauth's unexported scopeGranted helper
// a second time; trivial enough to keep local rather than exporting it just
// for this one call site.
func scopeGrantedLocal(granted []string, need string) bool {
	for _, g := range granted {
		if g == need {
			return true
		}
	}
	return false
}
