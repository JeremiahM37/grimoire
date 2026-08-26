package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// Every advertised tool must be classified.
//
// The annotations are what let a client auto-approve a read and confirm a
// delete without asking a person about both. A tool with no entry ships
// unannotated, which a cautious client reads as "unknown" and a careless one as
// "harmless" — and `forget` defaulting to harmless is the failure worth
// preventing.
func TestEveryToolIsClassified(t *testing.T) {
	for _, tl := range Tools() {
		if tl.Annotations == nil {
			t.Errorf("%s has no behaviour annotation — add it to the behaviour "+
				"table so clients know whether calling it is safe", tl.Name)
		}
	}
	// And nothing classified that is not advertised: a stale entry is a claim
	// about a tool nobody can call.
	advertised := map[string]bool{}
	for _, tl := range Tools() {
		advertised[tl.Name] = true
	}
	for name := range behaviour {
		if !advertised[name] {
			t.Errorf("behaviour table classifies %q, which is not an advertised tool", name)
		}
	}
}

// The classification has to be RIGHT, not merely present. These are the ones
// where being wrong has a cost.
func TestDangerousToolsAreNotMarkedSafe(t *testing.T) {
	byName := map[string]*annotations{}
	for _, tl := range Tools() {
		byName[tl.Name] = tl.Annotations
	}

	// Nothing that writes may claim to be read-only.
	for _, name := range []string{"create_note", "update_note", "remember", "forget",
		"set_fact", "append_daily", "consolidate_memory", "use_credential"} {
		a := byName[name]
		if a == nil {
			t.Fatalf("%s is unclassified", name)
		}
		if a.ReadOnlyHint {
			t.Errorf("%s is marked read-only but it writes", name)
		}
	}

	// Removal and replacement must be flagged destructive, or a client will
	// approve them the way it approves a search.
	for _, name := range []string{"forget", "update_note", "consolidate_memory"} {
		if !byName[name].DestructiveHint {
			t.Errorf("%s can destroy something on file and is not marked destructive", name)
		}
	}

	// use_credential spends a secret against a third party. Read-only would be
	// wrong twice over: it has an effect, and the effect is off this machine.
	if a := byName["use_credential"]; a.ReadOnlyHint || !a.OpenWorldHint {
		t.Errorf("use_credential: readOnly=%v openWorld=%v — it makes a billed, "+
			"audited call to another service", a.ReadOnlyHint, a.OpenWorldHint)
	}

	// Reads must not be flagged destructive, or every one of them prompts.
	for _, name := range []string{"search_notes", "read_note", "recall", "get_fact"} {
		a := byName[name]
		if !a.ReadOnlyHint || a.DestructiveHint {
			t.Errorf("%s: readOnly=%v destructive=%v — a read that prompts is a "+
				"read agents stop making", name, a.ReadOnlyHint, a.DestructiveHint)
		}
	}

	// Anything reaching the network must say so.
	for _, name := range []string{"search_web", "open_urls", "use_credential"} {
		if !byName[name].OpenWorldHint {
			t.Errorf("%s leaves this machine and is not marked open-world", name)
		}
	}
}

// The wire format matters: a client reads `annotations` off tools/list.
func TestAnnotationsAreOnTheWire(t *testing.T) {
	s := New("http://127.0.0.1:9111", "test")
	resp := s.handle(request{JSONRPC: "2.0", ID: []byte("1"), Method: "tools/list"})
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"annotations"`, `"readOnlyHint"`, `"destructiveHint"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("tools/list does not carry %s", want)
		}
	}
}
