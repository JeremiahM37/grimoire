package mcp

import (
	"encoding/json"
	"net/http"
	"testing"
)

// The importance an agent gives a fact reaches the API as a number, and the
// schema tells the model the range it may use.

func TestRememberForwardsImportanceAndOmitsItWhenUnrated(t *testing.T) {
	var body map[string]any
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true}`))
	})
	remember := func(args map[string]any) {
		body = nil
		call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "remember", "arguments": args}})
	}
	remember(map[string]any{"text": "x", "importance": 5})
	if body["importance"] != float64(5) {
		t.Errorf("importance = %v, want 5", body["importance"])
	}
	remember(map[string]any{"text": "x"})
	if _, present := body["importance"]; present {
		t.Errorf("an unrated write sent importance %v", body["importance"])
	}
}

func TestRememberSchemaBoundsImportanceToOneToFive(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	tools := resps[0]["result"].(map[string]any)["tools"].([]any)
	for _, tl := range tools {
		m := tl.(map[string]any)
		if m["name"] != "remember" {
			continue
		}
		props := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
		imp, ok := props["importance"].(map[string]any)
		if !ok {
			t.Fatal("remember does not advertise importance")
		}
		if imp["minimum"] != float64(1) || imp["maximum"] != float64(5) {
			t.Errorf("importance bounds = %v..%v, want 1..5", imp["minimum"], imp["maximum"])
		}
		return
	}
	t.Fatal("remember tool not listed")
}
