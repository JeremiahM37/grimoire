package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/readlog"
)

// withReads gives a test server a real audit log.
func withReads(t *testing.T, s *Server) {
	t.Helper()
	s.Reads = readlog.New(s.Index.DB)
	s.Reads.Start()
	t.Cleanup(s.Reads.Close)
}

func recorded(t *testing.T, s *Server, q readlog.Query) []readlog.Row {
	t.Helper()
	s.Reads.Flush()
	rows, err := s.Reads.Recent(q)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// The question the audit trail exists to answer: after the fact, who opened a
// restricted document — and who tried and could not.
func TestRestrictedReadsAreRecordedBothWays(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
		"path": "users/bob/diary.md", "body": "# Diary\n\nkestrel"}); w.Code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", w.Code, w.Body)
	}

	// Bob opens his own restricted note: recorded, allowed.
	if w := asKey(t, h, bobKey, "GET", "/api/notes/users/bob/diary.md", nil); w.Code != http.StatusOK {
		t.Fatalf("bob reading his own note = %d", w.Code)
	}
	// Alice, an administrator, opens it: recorded too. An administrator who
	// can read everything is precisely the account whose reads want a record.
	if w := asKey(t, h, aliceKey, "GET", "/api/notes/users/bob/diary.md", nil); w.Code != http.StatusOK {
		t.Fatalf("alice reading bob's note = %d", w.Code)
	}
	// A third party's failed attempt: recorded, denied.
	carolKey := makeUser(t, s, h, aliceKey, "carol", "member")
	if w := asKey(t, h, carolKey, "GET", "/api/notes/users/bob/diary.md", nil); w.Code != http.StatusNotFound {
		t.Fatalf("carol reading bob's note = %d", w.Code)
	}

	rows := recorded(t, s, readlog.Query{Path: "users/bob/diary.md"})
	if len(rows) != 3 {
		t.Fatalf("want 3 recorded reads, got %d: %+v", len(rows), rows)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Name] = r.Allowed
		if r.Route == "" {
			t.Errorf("no route recorded: %+v", r)
		}
	}
	if !seen["bob"] || !seen["alice"] {
		t.Errorf("allowed reads missing or marked denied: %+v", rows)
	}
	if v, ok := seen["carol"]; !ok || v {
		t.Errorf("carol's denied attempt not recorded as denied: %+v", rows)
	}

	denied := recorded(t, s, readlog.Query{Denied: true})
	if len(denied) != 1 || denied[0].Name != "carol" {
		t.Fatalf("denied filter = %+v", denied)
	}
}

// The trail must not become a log of everything everybody reads. An ordinary
// note in the commons is not an access event.
func TestOrdinaryReadsAreNotRecorded(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "handbook.md", "body": "# Handbook\n\nopen to all"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	for i := 0; i < 3; i++ {
		if w := asKey(t, h, aliceKey, "GET", "/api/notes/handbook.md", nil); w.Code != http.StatusOK {
			t.Fatalf("read = %d", w.Code)
		}
	}
	if rows := recorded(t, s, readlog.Query{}); len(rows) != 0 {
		t.Fatalf("commons reads were audited: %+v", rows)
	}
}

// A single-user deployment has nothing to restrict and nobody to restrict it
// from, so it must write nothing at all.
func TestSingleUserWritesNoAuditTrail(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	if w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "note.md", "body": "# Note\n\nbody"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/notes/note.md", nil); w.Code != http.StatusOK {
		t.Fatalf("read = %d", w.Code)
	}
	if rows := recorded(t, s, readlog.Query{}); len(rows) != 0 {
		t.Fatalf("single-user deployment audited a read: %+v", rows)
	}
}

// The trail says which people looked at which sensitive documents, so reading
// it is administration, not membership.
func TestAuditTrailIsAdminOnly(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := do(t, h, "GET", "/api/admin/reads", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous = %d, want 401", w.Code)
	}
	if w := asKey(t, h, bobKey, "GET", "/api/admin/reads", nil); w.Code != http.StatusForbidden {
		t.Fatalf("member = %d, want 403", w.Code)
	}
	w := asKey(t, h, aliceKey, "GET", "/api/admin/reads", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("admin = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"reads"`) {
		t.Fatalf("unexpected body: %s", w.Body)
	}
}

// A vault export copies restricted documents out wholesale; each one is an
// access, and the trail would be misleading if the bulk path skipped it.
func TestVaultExportRecordsEachRestrictedDocument(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	for _, p := range []string{"users/alice/one.md", "users/alice/two.md"} {
		if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
			"path": p, "body": "# X\n\nbody"}); w.Code != http.StatusCreated {
			t.Fatalf("create %s = %d %s", p, w.Code, w.Body)
		}
	}
	if w := asKey(t, h, aliceKey, "GET", "/api/export/vault", nil); w.Code != http.StatusOK {
		t.Fatalf("export = %d", w.Code)
	}
	rows := recorded(t, s, readlog.Query{Path: "users/alice/"})
	if len(rows) < 2 {
		t.Fatalf("export recorded %d of 2 restricted documents: %+v", len(rows), rows)
	}
}
