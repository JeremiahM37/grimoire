package mcp

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The remember tool carries anchors and event to the API, and tells the API
// when the caller is remote, because the bridge itself is always loopback.

func rememberCall(args map[string]any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "remember", "arguments": args}}
}

func TestRememberForwardsAnchorsAndEvent(t *testing.T) {
	var body map[string]any
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true}`))
	})
	anchor := map[string]any{"type": "file_contains", "path": "/home/admin/lan.txt", "text": "ROUTER_IP"}
	call(t, s, rememberCall(map[string]any{"text": "x", "anchors": []any{anchor, "not-an-object"}}))
	list, ok := body["anchors"].([]any)
	if !ok || len(list) != 1 || list[0].(map[string]any)["type"] != "file_contains" {
		t.Fatalf("anchors = %v, want the one object", body["anchors"])
	}
	if _, present := body["event"]; present {
		t.Errorf("event was sent though the caller did not set it")
	}

	call(t, s, rememberCall(map[string]any{"text": "the cutover happened", "event": true}))
	if body["event"] != true {
		t.Errorf("event = %v, want true", body["event"])
	}
	if _, present := body["anchors"]; present {
		t.Errorf("an unset anchors list was sent: %v", body["anchors"])
	}
}

func TestLocalStdioRememberIsNotMarkedRemote(t *testing.T) {
	var body map[string]any
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true}`))
	})
	call(t, s, rememberCall(map[string]any{"text": "x", "anchors": []any{map[string]any{"type": "remote"}}}))
	if _, present := body["caller_remote"]; present {
		t.Fatalf("a stdio call was marked remote: %v", body)
	}
}

func TestHTTPRememberIsMarkedRemoteEvenWhenArgumentsSayItIsNot(t *testing.T) {
	var body map[string]any
	api := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		body = nil
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"ok":true}`))
	})
	h := api.HTTPHandler()
	// A caller can send its own copy of the reserved key. It must not be able
	// to take the mark off: the server sets it for every HTTP call.
	raw := rememberCall(map[string]any{"text": "x", "anchors": []any{map[string]any{"type": "remote"}},
		remoteCallerArg: false})
	b, _ := json.Marshal(raw)
	req := httptest.NewRequest("POST", "/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = "127.0.0.1:40000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("POST /mcp = %d: %s", w.Code, w.Body)
	}
	if body["caller_remote"] != true {
		t.Fatalf("HTTP call not marked remote: %v", body)
	}
}

func TestRememberSchemaAdvertisesAnchorsAndEvent(t *testing.T) {
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {})
	resps := call(t, s, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list"})
	for _, tl := range resps[0]["result"].(map[string]any)["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"] != "remember" {
			continue
		}
		props := m["inputSchema"].(map[string]any)["properties"].(map[string]any)
		for _, k := range []string{"anchors", "event"} {
			if _, ok := props[k]; !ok {
				t.Errorf("remember does not advertise %s", k)
			}
		}
		desc, _ := m["description"].(string)
		for _, probe := range []string{"file_exists", "file_contains", "port_closed", "git_branch", "remote"} {
			if !strings.Contains(desc, probe) {
				t.Errorf("remember description omits probe type %s", probe)
			}
		}
		if !strings.Contains(desc, "event") || !strings.Contains(desc, "preference") || !strings.Contains(desc, "claim") {
			t.Errorf("remember description omits the three kinds")
		}
		return
	}
	t.Fatal("remember tool not listed")
}

func TestRememberAnchorsDescriptionIsShort(t *testing.T) {
	if n := len(strings.Fields(rememberAnchors)); n >= 120 {
		t.Fatalf("anchor description is %d words, must be under 120", n)
	}
}
