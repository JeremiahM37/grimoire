package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// Two real servers, two real vaults, talking over HTTP. Sync is the one feature
// whose bugs destroy data, so a unit test of the diff logic is not enough — the
// wire format has to be exercised end to end.
func twoPeers(t *testing.T) (a *Server, ah http.Handler, b *Server, bh http.Handler, bURL string) {
	t.Helper()
	a, ah = testServer(t)
	b, bh = testServer(t)
	srv := httptest.NewServer(bh)
	t.Cleanup(srv.Close)
	a.SyncPeer = srv.URL
	return a, ah, b, bh, srv.URL
}

func TestSyncMovesNotesBothWays(t *testing.T) {
	_, ah, _, bh, _ := twoPeers(t)
	do(t, ah, "POST", "/api/notes", map[string]any{
		"path": "mine.md", "body": "# Mine\n\nwritten on A\n"})
	do(t, bh, "POST", "/api/notes", map[string]any{
		"path": "theirs.md", "body": "# Theirs\n\nwritten on B\n"})

	w := do(t, ah, "POST", "/api/sync/now", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("sync = %d: %s", w.Code, w.Body)
	}
	var st map[string]any
	decode(t, w, &st)

	// A should now hold B's note, and B should hold A's
	if w := do(t, ah, "GET", "/api/notes/theirs.md", nil); w.Code != http.StatusOK {
		t.Errorf("A did not pull theirs.md: %d %s (stats %v)", w.Code, w.Body, st)
	}
	if w := do(t, bh, "GET", "/api/notes/mine.md", nil); w.Code != http.StatusOK {
		t.Errorf("B did not receive mine.md: %d %s (stats %v)", w.Code, w.Body, st)
	}
}

func TestSyncNowRefusesWithoutAPeer(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/sync/now", nil); w.Code != http.StatusBadRequest {
		t.Errorf("sync with no peer = %d, want 400", w.Code)
	}
}

func TestManifestIsKeyedByPath(t *testing.T) {
	// the wire format a peer of either implementation expects: {path: {hash, mtime}}
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "a.md", "body": "# A\n"})

	var m map[string]map[string]any
	decode(t, do(t, h, "GET", "/api/sync/manifest", nil), &m)
	entry, ok := m["a.md"]
	if !ok {
		t.Fatalf("manifest = %v", m)
	}
	if entry["hash"] == "" || entry["hash"] == nil {
		t.Errorf("no hash in %v", entry)
	}
	if _, ok := entry["mtime"]; !ok {
		t.Errorf("no mtime in %v — a peer decides direction by it", entry)
	}
}

func TestPushWithAStaleBaseHashMakesAConflictCopy(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"path": "shared.md", "body": "# Shared\n\nthe server's version\n"})

	// a peer pushing from a base it no longer has must never overwrite
	stale := "0000000000000000"
	incoming := "---\ntitle: Shared\n---\n# Shared\n\nthe peer's version\n"
	var out map[string]any
	decode(t, do(t, h, "POST", "/api/sync/push", map[string]any{
		"changes": []map[string]any{
			{"path": "shared.md", "content": incoming, "base_hash": stale}},
		"device": "test"}), &out)

	results := out["results"].([]any)
	first := results[0].(map[string]any)
	if first["status"] != "conflict" {
		t.Fatalf("status = %v, want conflict", first["status"])
	}
	copyRel, _ := first["conflict_copy"].(string)
	if copyRel == "" {
		t.Fatal("no conflict copy path returned")
	}

	var kept map[string]any
	decode(t, do(t, h, "GET", "/api/notes/shared.md", nil), &kept)
	if !strings.Contains(kept["body"].(string), "the server's version") {
		t.Error("the server's copy was overwritten — sync must never lose data")
	}
	var copied map[string]any
	// the conflict name carries a timestamp in parentheses and spaces — a real
	// client escapes it, and so must this one
	decode(t, do(t, h, "GET", "/api/notes/"+(&url.URL{Path: copyRel}).EscapedPath(), nil), &copied)
	if !strings.Contains(copied["body"].(string), "the peer's version") {
		t.Error("the incoming version was not preserved in the conflict copy")
	}
}

func TestPullReportsMissingNotesAsNull(t *testing.T) {
	_, h := testServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{"path": "here.md", "body": "# Here\n"})

	var out map[string]map[string]*string
	decode(t, do(t, h, "POST", "/api/sync/pull",
		map[string]any{"paths": []string{"here.md", "gone.md"}}), &out)
	contents := out["contents"]
	if contents["here.md"] == nil || !strings.Contains(*contents["here.md"], "# Here") {
		t.Errorf("here.md = %v", contents["here.md"])
	}
	if v, ok := contents["gone.md"]; !ok || v != nil {
		// null distinguishes "deleted on the peer" from "never asked for"
		t.Errorf("gone.md = %v, want an explicit null", v)
	}
}
