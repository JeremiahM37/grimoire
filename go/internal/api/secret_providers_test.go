package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvidersAndLinksNeverExposeValues(t *testing.T) {
	_, h := vaultedServer(t)
	dir := t.TempDir()
	script := "#!/bin/sh\n[ \"$OP_SERVICE_ACCOUNT_TOKEN\" = \"ops_secret_tok\" ] || exit 1\nprintf 'resolved-api-secret'\n"
	if err := os.WriteFile(filepath.Join(dir, "op"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	w := do(t, h, "POST", "/api/secrets/providers", map[string]any{
		"name": "op", "kind": "onepassword",
		"secrets": map[string]string{"service_account_token": "ops_secret_tok"}})
	if w.Code != http.StatusCreated {
		t.Fatalf("add provider = %d: %s", w.Code, w.Body)
	}
	if w = do(t, h, "POST", "/api/secrets/link", map[string]any{"name": "gh", "ref": "op://a/b/c"}); w.Code != http.StatusCreated {
		t.Fatalf("link = %d: %s", w.Code, w.Body)
	}
	if w = do(t, h, "POST", "/api/secrets/providers/op/test", nil); !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("test: %s", w.Body)
	}
	if w = do(t, h, "POST", "/api/secrets/gh/grant", map[string]any{"grantee": "agent", "scope": "https://example.com"}); w.Code != 200 {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/api/secrets/providers", "/api/secrets", "/api/secrets/details", "/api/grants", "/api/audit"} {
		w = do(t, h, "GET", path, nil)
		body := w.Body.String()
		if w.Code != 200 || strings.Contains(body, "ops_secret_tok") || strings.Contains(body, "resolved-api-secret") {
			t.Fatalf("%s = %d leaks or fails: %s", path, w.Code, body)
		}
	}
	if w = do(t, h, "GET", "/api/grants", nil); !strings.Contains(w.Body.String(), `"provider":"op"`) ||
		!strings.Contains(w.Body.String(), `op://a/b/c`) {
		t.Fatalf("grants should show provider+ref: %s", w.Body)
	}
	if w = do(t, h, "DELETE", "/api/secrets/providers/op", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("removing a linked provider = %d", w.Code)
	}
}
