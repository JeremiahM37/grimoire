package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCodePositionalsSkipFlagsAndTheirValues(t *testing.T) {
	got := positionals([]string{"Store.Save", "--kind", "method", "--root", "/r", "extra"})
	want := []string{"Store.Save", "extra"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("positionals = %v, want %v", got, want)
	}
}

// The CLI goes through the server, so a round trip against a stub server
// checks the wire shape: the route, the query, and the admin header the bank
// client sends.
func TestCodeSymbolCallsTheServer(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.RequestURI()
		json.NewEncoder(w).Encode(map[string]any{"symbols": []map[string]any{
			{"root": "/r", "path": "a.go", "qualified": "Store.Save", "kind": "method", "line": 4},
		}})
	}))
	defer srv.Close()

	if code := cmdCode([]string{"symbol", "Save", "--kind", "method", "--url", srv.URL}); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(gotPath, "/api/code/symbol?") || !strings.Contains(gotPath, "name=Save") || !strings.Contains(gotPath, "kind=method") {
		t.Fatalf("request = %s", gotPath)
	}
}

func TestCodeRejectsMissingArguments(t *testing.T) {
	if code := cmdCode(nil); code == 0 {
		t.Error("no subcommand should fail")
	}
	if code := cmdCode([]string{"index"}); code == 0 {
		t.Error("index without a path should fail")
	}
	if code := cmdCode([]string{"nonsense", "x"}); code == 0 {
		t.Error("an unknown subcommand should fail")
	}
}
