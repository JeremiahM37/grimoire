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

// An answer that quotes a restricted document has disclosed it. Searching is
// not recorded, but being shown the text is.
func TestAnsweredCitationsOfRestrictedNotesAreRecorded(t *testing.T) {
	s, h := testServer(t)
	withReads(t, s)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/secret.md", "body": "# Secret\n\nthe kestrel roosts at dawn"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	// A search alone is not an access, and must leave no record.
	if w := asKey(t, h, aliceKey, "GET", "/api/search?q=kestrel", nil); w.Code != http.StatusOK {
		t.Fatalf("search = %d", w.Code)
	}
	if rows := recorded(t, s, readlog.Query{}); len(rows) != 0 {
		t.Fatalf("search was audited: %+v", rows)
	}

	w := asKey(t, h, aliceKey, "POST", "/api/ask", map[string]any{"q": "kestrel", "k": 5})
	if w.Code != http.StatusOK {
		t.Fatalf("ask = %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "users/alice/secret.md") {
		t.Skip("no citation of the restricted note; nothing to audit")
	}
	rows := recorded(t, s, readlog.Query{Path: "users/alice/secret.md"})
	if len(rows) == 0 {
		t.Fatal("a restricted note was quoted in an answer without being recorded")
	}
	if !strings.Contains(rows[0].Route, "/api/ask") {
		t.Fatalf("route not recorded as the answering one: %+v", rows[0])
	}
}

// A query block lists notes, so it is a read — and it must obey the same rules
// as every other read.
//
// This is the behaviour test for a hole that shipped: /api/query answered
// anyone and returned unfiltered rows, because "authenticated surface" was
// true of a server that had exactly one account. The route-classification
// table records that this route is scoped; only this test proves it acts that
// way, so a filter deleted in a refactor fails here rather than in someone's
// vault.
func TestQueryBlocksAreFilteredToWhatTheCallerMaySee(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	for _, p := range []string{"users/alice/private.md", "users/bob/own.md"} {
		if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
			"path": p, "body": "# N\n\ntagged\n\n#shared"}); w.Code != http.StatusCreated {
			// bob's note is created by alice only if she may write it; do it as
			// its owner instead when she may not.
			if w2 := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
				"path": p, "body": "# N\n\ntagged\n\n#shared"}); w2.Code != http.StatusCreated {
				t.Fatalf("create %s = %d %s / %d %s", p, w.Code, w.Body, w2.Code, w2.Body)
			}
		}
	}

	block := map[string]any{"block": "tag: shared"}

	// Anonymous callers get no listing at all.
	if w := do(t, h, "POST", "/api/query", block); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous query = %d %s, want 401", w.Code, w.Body)
	}

	// Bob sees his own note and not Alice's.
	w := asKey(t, h, bobKey, "POST", "/api/query", block)
	if w.Code != http.StatusOK {
		t.Fatalf("bob query = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, "users/alice/private.md") {
		t.Fatalf("query leaked alice's note to bob: %s", body)
	}
	if !strings.Contains(body, "users/bob/own.md") {
		t.Fatalf("query hid bob's own note from him: %s", body)
	}

	// The count must agree with the rows; a filtered list that still reports
	// the unfiltered total tells the caller how much they are not seeing.
	var out struct {
		Rows  []map[string]any `json:"rows"`
		Count int              `json:"count"`
	}
	decode(t, w, &out)
	if out.Count != len(out.Rows) {
		t.Fatalf("count %d does not match %d returned rows", out.Count, len(out.Rows))
	}
}

// The vault import writes notes from an uploaded archive. It answered anyone.
func TestVaultImportRequiresAnAccount(t *testing.T) {
	_, h := testServer(t)
	s2, h2 := testServer(t)
	makeUser(t, s2, h2, "", "alice", "admin")

	// Single-user: still open, exactly as before.
	if w := do(t, h, "POST", "/api/import/vault", nil); w.Code == http.StatusUnauthorized {
		t.Fatal("single-user deployment now demands a login for import")
	}
	// Multi-user: anonymous writes are refused before any archive is read.
	if w := do(t, h2, "POST", "/api/import/vault", nil); w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous import = %d %s, want 401", w.Code, w.Body)
	}
}
