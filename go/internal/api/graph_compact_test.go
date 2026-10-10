package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGraphCompactAndETag(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "gw.md", "body": "# Gateway\n\nport #infra"})
	do(t, h, "POST", "/api/notes", map[string]any{"path": "hub.md", "body": "# Hub\n\nsee [[Gateway]] #infra"})

	w := do(t, h, "GET", "/api/graph?compact=1", nil)
	var g struct {
		V     int        `json:"v"`
		IDs   []string   `json:"ids"`
		T     []int64    `json:"t"`
		Tags  [][]string `json:"tags"`
		Edges []int      `json:"edges"`
	}
	decode(t, w, &g)
	if g.V != 2 || len(g.IDs) != 2 || len(g.T) != 2 || len(g.Edges) != 2 || g.T[0] == 0 {
		t.Fatalf("compact graph = %+v", g)
	}
	if len(g.Tags[0]) != 1 || g.Tags[0][0] != "infra" {
		t.Errorf("tags = %v", g.Tags)
	}
	etag := w.Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	req := httptest.NewRequest("GET", "/api/graph?compact=1", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified || rec.Body.Len() != 0 {
		t.Errorf("revalidation = %d with %d body bytes", rec.Code, rec.Body.Len())
	}
	// the legacy object form still works and has its own tag
	w = do(t, h, "GET", "/api/graph", nil)
	if w.Header().Get("ETag") == etag {
		t.Error("object and compact forms share an ETag")
	}
}
