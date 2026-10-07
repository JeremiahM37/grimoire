package mcp

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type seenReq struct {
	method, path string
	body         map[string]any
}

func bankStub(t *testing.T) (*Server, *[]seenReq) {
	t.Helper()
	var seen []seenReq
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, seenReq{r.Method, r.URL.RequestURI(), body})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	s.Session, s.Bank = "", ""
	return s, &seen
}

func TestBankToolsUseTheNamedOrDefaultBank(t *testing.T) {
	s, seen := bankStub(t)
	if _, err := s.dispatch("bank_recall", map[string]any{"query": "x"}); err == nil ||
		!strings.Contains(err.Error(), EnvBank) {
		t.Errorf("no bank anywhere must say how to set one: %v", err)
	}
	s.Bank = "team:alpha"
	if _, err := s.dispatch("bank_recall", map[string]any{"query": "who owns billing", "max_tokens": 500.0,
		"tags": []any{"ops"}, "include_chunks": true}); err != nil {
		t.Fatal(err)
	}
	got := (*seen)[0]
	if got.method != "POST" || got.path != "/api/banks/team:alpha/memories/recall" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	if got.body["query"] != "who owns billing" || got.body["max_tokens"] != 500.0 || got.body["include"] == nil {
		t.Errorf("body = %v", got.body)
	}
	if _, err := s.dispatch("retain", map[string]any{"bank": "other", "content": "Dana: hi", "document_id": "d1",
		"tags": []any{"a"}}); err != nil {
		t.Fatal(err)
	}
	got = (*seen)[1]
	item := got.body["items"].([]any)[0].(map[string]any)
	if got.path != "/api/banks/other/memories" || item["document_id"] != "d1" || item["content"] != "Dana: hi" {
		t.Errorf("retain = %s %v", got.path, got.body)
	}
}

func TestRetainFilesARunsContentUnderItsSession(t *testing.T) {
	s, seen := bankStub(t)
	s.Bank, s.Session = "b", "run-7"
	if _, err := s.dispatch("retain", map[string]any{"content": "a transcript"}); err != nil {
		t.Fatal(err)
	}
	item := (*seen)[0].body["items"].([]any)[0].(map[string]any)
	if item["document_id"] != "run-7" || item["update_mode"] != "append" {
		t.Errorf("item = %v", item)
	}
}

func TestBankDeletesPassForceOnlyWhenAsked(t *testing.T) {
	s, seen := bankStub(t)
	s.Bank = "b"
	_, _ = s.dispatch("delete_bank_memory", map[string]any{"id": "f1"})
	_, _ = s.dispatch("delete_bank_document", map[string]any{"document_id": "a/b", "force": true})
	if (*seen)[0].path != "/api/banks/b/memories/f1" || (*seen)[1].path != "/api/banks/b/documents/a%2Fb?force=true" {
		t.Errorf("paths = %s, %s", (*seen)[0].path, (*seen)[1].path)
	}
}

func TestProgressiveDisclosureToolsUseTheIndexTimelineAndLookupRoutes(t *testing.T) {
	var seen []string
	s := stubAPI(t, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.RequestURI())
		switch {
		case strings.Contains(r.URL.Path, "/index"), strings.Contains(r.URL.Path, "/timeline"):
			_, _ = w.Write([]byte(`{"total":2,"items":[{"ref":"#f3a9c1b2","type":"fact","title":"Cache is keyed by commit","date":"2026-10-01","tokens":9,"human":true},` +
				`{"ref":"#o77aa001","type":"observation","title":"Releases are tagged from main","tokens":7}]}`))
		default:
			_, _ = w.Write([]byte(`{"items":[],"missing":["zzzz"]}`))
		}
	})
	s.Session, s.Bank = "", "b"
	r, err := s.dispatch("bank_index", map[string]any{"query": "cache", "types": []any{"fact"}, "limit": 5.0})
	if err != nil {
		t.Fatal(err)
	}
	if seen[0] != "/api/banks/b/index?limit=5&q=cache&types=fact" {
		t.Errorf("index request %q", seen[0])
	}
	lines := r.(map[string]any)["entries"].([]string)
	if lines[0] != "#f3a9c1b2 2026-10-01 fact ~9t  Cache is keyed by commit [person]" || !strings.Contains(lines[1], "undated") {
		t.Errorf("lines = %q", lines)
	}
	if _, err := s.dispatch("bank_timeline", map[string]any{"anchor": "#f3a9c1b2", "before": 2.0}); err != nil {
		t.Fatal(err)
	}
	if seen[1] != "/api/banks/b/timeline?anchor=%23f3a9c1b2&before=2" {
		t.Errorf("timeline request %q", seen[1])
	}
	if _, err := s.dispatch("bank_get", map[string]any{"ids": []any{"#f3a9c1b2", "o77aa001"}}); err != nil {
		t.Fatal(err)
	}
	if seen[2] != "/api/banks/b/lookup?ids=%23f3a9c1b2%2Co77aa001" {
		t.Errorf("lookup request %q", seen[2])
	}
}
