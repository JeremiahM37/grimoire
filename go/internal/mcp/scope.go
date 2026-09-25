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
	"memory_graph":       oauth.ScopeMemory,
	"memory_feedback":    oauth.ScopeMemory,
	"memory_scopes":      oauth.ScopeMemory,
	"forget":             oauth.ScopeMemory,
	"consolidate_memory": oauth.ScopeMemory,

	// The credential broker. Kept apart from everything else — see
	// oauth.DefaultScopes — because this is the one scope that can spend a
	// credential against a third party on the owner's behalf.
	"list_grants":              oauth.ScopeCredentials,
	"use_credential":           oauth.ScopeCredentials,
	"request_credential":       oauth.ScopeCredentials,
	"check_credential_request": oauth.ScopeCredentials,
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
