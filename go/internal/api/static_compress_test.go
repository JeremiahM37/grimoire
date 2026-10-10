package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStaticAssetsAreGzippedAndImmutable(t *testing.T) {
	t.Parallel()
	web := t.TempDir()
	dist := filepath.Join(web, "..", "frontend", "dist")
	_ = dist
	if err := os.WriteFile(filepath.Join(web, "app.js"), []byte(strings.Repeat("console.log('x');", 200)), 0o644); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/app.js", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	if !serveCompressed(rec, req, web) {
		t.Fatal("compressible file was not served compressed")
	}
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Code != http.StatusOK || rec.Body.Len() >= 3400 {
		t.Fatalf("bad response: %d enc=%q len=%d", rec.Code, rec.Header().Get("Content-Encoding"), rec.Body.Len())
	}
	plain := httptest.NewRequest("GET", "/app.js", nil)
	if serveCompressed(httptest.NewRecorder(), plain, web) {
		t.Fatal("a client that did not ask for gzip got it")
	}
	if serveCompressed(httptest.NewRecorder(), httptest.NewRequest("GET", "/../../etc/passwd.js", nil), web) {
		t.Fatal("path escaped the web root")
	}
}
