package api

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The corpus-fits path is a size check, so its tests are about the boundary:
// below the budget the whole corpus comes back, above it retrieval does, and
// the switch never smuggles a private note across.

func seedNotes(t *testing.T, h http.Handler, n int, body string) {
	t.Helper()
	for i := 0; i < n; i++ {
		do(t, h, "POST", "/api/notes", map[string]any{
			"path": fmt.Sprintf("n%03d.md", i),
			"body": fmt.Sprintf("# Note %d\n\n%s\n\nmarker%03d", i, body, i)})
	}
}

func TestSmallCorpusIsReturnedWholeInsteadOfRanked(t *testing.T) {
	_, h := testServer(t)
	seedNotes(t, h, 12, "kestrel plumage notes about falconry and weather")

	w := do(t, h, "GET", "/api/context?q=kestrel&k=3&budget=1000000", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("context = %d: %s", w.Code, w.Body)
	}
	var got struct {
		Mode     string `json:"mode"`
		Passages []struct {
			Path  string `json:"path"`
			Chunk string `json:"chunk"`
		} `json:"passages"`
	}
	decode(t, w, &got)
	if got.Mode != "full" {
		t.Fatalf("mode = %q, want full for a corpus far under budget", got.Mode)
	}
	// every note must be represented, including ones a top-3 ranking would drop
	body := w.Body.String()
	for i := 0; i < 12; i++ {
		if !strings.Contains(body, fmt.Sprintf("marker%03d", i)) {
			t.Errorf("note %d missing from the whole-corpus context", i)
		}
	}
	if len(got.Passages) <= 3 {
		t.Errorf("got %d passages for k=3; the point is that k stops applying "+
			"when everything fits", len(got.Passages))
	}
}

func TestCorpusOverBudgetFallsBackToRetrieval(t *testing.T) {
	_, h := testServer(t)
	seedNotes(t, h, 12, strings.Repeat("kestrel plumage falconry weather ", 40))

	w := do(t, h, "GET", "/api/context?q=kestrel&k=3&budget=200", nil)
	var got struct {
		Mode     string `json:"mode"`
		Passages []any  `json:"passages"`
	}
	decode(t, w, &got)
	if got.Mode != "retrieved" {
		t.Fatalf("mode = %q, want retrieved for a corpus over budget", got.Mode)
	}
	if len(got.Passages) > 3 {
		t.Errorf("retrieval returned %d passages for k=3", len(got.Passages))
	}
}

// A budget of zero disables the shortcut entirely, which is how a deployment
// that never wants to pay for whole-corpus context turns it off.
func TestZeroBudgetAlwaysRetrieves(t *testing.T) {
	_, h := testServer(t)
	seedNotes(t, h, 5, "kestrel plumage")
	var got struct {
		Mode string `json:"mode"`
	}
	decode(t, do(t, h, "GET", "/api/context?q=kestrel&k=2&budget=0", nil), &got)
	if got.Mode != "retrieved" {
		t.Errorf("mode = %q with budget=0, want retrieved", got.Mode)
	}
}

// The whole-corpus path bypasses ranking, and ranking is where the private
// filter lived. This is the test that the shortcut did not open a hole.
func TestWholeCorpusStillExcludesPrivateNotes(t *testing.T) {
	srv, h := testServer(t)
	seedNotes(t, h, 4, "kestrel plumage")
	// written straight into the vault, the way an external editor or a sync
	// client creates a note — which is also the path that actually parses
	// `private:` out of the frontmatter
	if err := os.WriteFile(filepath.Join(srv.Vault.Root, "secret.md"),
		[]byte("---\nprivate: true\ntitle: Secret\n---\n\nkestrel PRIVATEMARKER\n"),
		0o644); err != nil {
		t.Fatal(err)
	}
	do(t, h, "POST", "/api/reindex", nil)

	var n int
	if err := srv.Index.DB.QueryRow(
		"SELECT private FROM notes WHERE path='secret.md'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("fixture note is not private (private=%d, err=%v); this test "+
			"would prove nothing", n, err)
	}

	w := do(t, h, "GET", "/api/context?q=kestrel&k=3&budget=1000000", nil)
	var got struct {
		Mode string `json:"mode"`
	}
	decode(t, w, &got)
	if got.Mode != "full" {
		t.Fatalf("mode = %q; the corpus should be under budget here", got.Mode)
	}
	if strings.Contains(w.Body.String(), "PRIVATEMARKER") {
		t.Fatal("a private note leaked into the whole-corpus context")
	}
	// and it comes back when the caller is entitled to it
	w = do(t, h, "GET", "/api/context?q=kestrel&k=3&budget=1000000&include_private=1", nil)
	if !strings.Contains(w.Body.String(), "PRIVATEMARKER") {
		t.Error("include_private did not include the private note")
	}
}
