package mcp

import (
	"encoding/json"
	"net/http"
	"testing"
)

// remember_image forwards the bytes and caption to the image endpoint, and the
// attribution is the server's own identity, as it is for remember.
func TestRememberImageForwardsToTheImageEndpoint(t *testing.T) {
	var gotPath string
	var body map[string]any
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body = nil
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"searchable":true}`))
	})
	call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "remember_image", "arguments": map[string]any{
			"data_base64": "iVBORw0KGgo=", "caption": "the rack diagram", "topic": "infra"}}})
	if gotPath != "/api/memory/image" {
		t.Fatalf("remember_image hit %q", gotPath)
	}
	if body["data_base64"] != "iVBORw0KGgo=" || body["caption"] != "the rack diagram" || body["topic"] != "infra" {
		t.Fatalf("forwarded body = %v", body)
	}
	if _, ok := body["agent"]; !ok {
		t.Fatal("attribution must travel with the write, as remember's does")
	}
}
