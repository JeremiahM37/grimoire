package mcp

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/oauth"
)

// Every advertised tool must have a scope, the same completeness rule
// TestEveryToolIsClassified already enforces for behaviour annotations — an
// unclassified tool would silently vanish from every scoped tools/list.
func TestEveryToolHasAScope(t *testing.T) {
	for _, tl := range Tools() {
		if _, ok := scopeFor(tl.Name); !ok {
			t.Errorf("%s has no OAuth scope classification — add it to toolScope", tl.Name)
		}
	}
	advertised := map[string]bool{}
	for _, tl := range Tools() {
		advertised[tl.Name] = true
	}
	for name := range toolScope {
		if !advertised[name] {
			t.Errorf("toolScope classifies %q, which is not an advertised tool", name)
		}
	}
}

// The credential broker must never end up reachable under any other scope —
// that is the one classification mistake here with real consequences.
func TestCredentialToolsAreScopedCredentials(t *testing.T) {
	for _, name := range []string{"list_grants", "use_credential", "request_credential", "check_credential_request"} {
		got, ok := scopeFor(name)
		if !ok || got != oauth.ScopeCredentials {
			t.Errorf("%s: want scope %q, got %q (classified=%v)", name, oauth.ScopeCredentials, got, ok)
		}
	}
}

func TestFilterToolsByScope(t *testing.T) {
	all := Tools()
	filtered := filterToolsByScope(all, []string{oauth.ScopeNotesRead})
	seen := map[string]bool{}
	for _, tl := range filtered {
		seen[tl.Name] = true
		need, _ := scopeFor(tl.Name)
		if need != oauth.ScopeNotesRead {
			t.Errorf("filterToolsByScope let %s through with scope %q for a notes:read-only grant", tl.Name, need)
		}
	}
	if !seen["search_notes"] {
		t.Error("search_notes should survive a notes:read filter")
	}
	if seen["use_credential"] {
		t.Error("use_credential must not survive a notes:read-only filter")
	}
	if seen["create_note"] {
		t.Error("create_note must not survive a notes:read-only filter")
	}
}
