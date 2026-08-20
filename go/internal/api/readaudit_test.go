package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
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

// Full-corpus mode is the most dangerous read path in the system, and until
// this test it was the least exercised one.
//
// When the whole corpus fits the context budget, the answering path stops
// ranking and hands the model EVERYTHING — the branch where nothing is scored
// and therefore nothing had to be checked. It only runs when an LLM is
// configured, and no other test configures one, so the branch that reads every
// note in the vault was never taken at the HTTP boundary.
//
// It is reached here by setting GRIMOIRE_LLM: the backend is then "available"
// and unreachable, so the answer falls back to quoting the passages it was
// handed — which is exactly what makes this a real check. If the filter were
// missing, the restricted text would appear in the answer body, not merely in
// a citation list.
func TestFullCorpusAnswersStillObeyAccess(t *testing.T) {
	t.Setenv("GRIMOIRE_LLM", "ollama")
	s, h := testServer(t)
	if !s.AI.Available() {
		t.Fatal("full-corpus mode is unreachable; this test would prove nothing")
	}
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/vault.md",
		"body": "# Vault\n\nthe kestrel combination is SEVENTEEN"}); w.Code != http.StatusCreated {
		t.Fatalf("alice create = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
		"path": "users/bob/notes.md", "body": "# Bob\n\nkestrel sightings"}); w.Code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", w.Code, w.Body)
	}
	// Both legs of the filter, because they fail independently: a note bob is
	// kept out of by SPACE, and one in the commons — which he can read — that
	// he is kept out of only by its READER LIST. An earlier version of this
	// test had only the first, and passed with the reader-list clause deleted.
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteNote("shared/minutes.md",
		"# Minutes\n\nkestrel LISTONLYMARKER decision", map[string]any{
			"title": "Minutes", "readers": alice.ID,
		}); err != nil {
		t.Fatal(err)
	}

	w := asKey(t, h, bobKey, "POST", "/api/ask", map[string]any{"q": "kestrel", "k": 10})
	if w.Code != http.StatusOK {
		t.Fatalf("ask = %d %s", w.Code, w.Body)
	}
	body := w.Body.String()
	if strings.Contains(body, "SEVENTEEN") {
		t.Fatalf("alice's space-restricted text reached bob through the answer: %s", body)
	}
	if strings.Contains(body, "LISTONLYMARKER") || strings.Contains(body, "shared/minutes.md") {
		t.Fatalf("a reader-list-restricted note reached bob through the answer: %s", body)
	}
	if strings.Contains(body, "users/alice/vault.md") {
		t.Fatalf("alice's note was cited to bob: %s", body)
	}

	// And the mode really was the unranked one, or the test proved the wrong
	// branch safe.
	var out struct {
		Mode string `json:"mode"`
	}
	decode(t, w, &out)
	if out.Mode != "full" {
		t.Fatalf("answered in %q mode, not the whole-corpus branch this test exists for", out.Mode)
	}
}

// The trash is a read surface wearing a different name, and it was open.
//
// GET /api/trash took the request as `_` — a handler that never looks at who
// is asking cannot be filtering — and returned every deleted note's original
// path and title to anyone, across every space. Restore put a note back where
// it came from without checking whether the caller may write there, and purge
// destroyed its last copy without checking anything at all.
func TestTrashDoesNotLeakOrAcceptOtherPeoplesNotes(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/severance-terms.md", "body": "# Terms\n\nconfidential"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, aliceKey, "DELETE", "/api/notes/users/alice/severance-terms.md", nil); w.Code != http.StatusOK {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}

	// The title and path of a deleted note are the sensitive part — which is
	// why a refused read reports "absent" rather than "forbidden" everywhere
	// else in this server.
	if body := asKey(t, h, bobKey, "GET", "/api/trash", nil).Body.String(); strings.Contains(body, "severance-terms") {
		t.Errorf("trash listing leaked alice's deleted note to bob: %s", body)
	}
	if w := do(t, h, "GET", "/api/trash", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous trash listing = %d, want 401", w.Code)
	}

	// Alice sees her own, or this is a broken trash rather than a private one.
	var entries []map[string]any
	decode(t, asKey(t, h, aliceKey, "GET", "/api/trash", nil), &entries)
	if len(entries) != 1 {
		t.Fatalf("alice sees %d of her own trashed notes", len(entries))
	}
	tid, _ := entries[0]["id"].(string)
	if tid == "" {
		t.Fatalf("no trash id in %v", entries[0])
	}

	// Bob may not restore it into her space, nor destroy her only copy.
	if w := asKey(t, h, bobKey, "POST", "/api/trash/"+tid+"/restore", nil); w.Code == http.StatusOK {
		t.Error("bob restored a note into a space he cannot write")
	}
	if w := asKey(t, h, bobKey, "DELETE", "/api/trash/"+tid, nil); w.Code < 400 {
		t.Errorf("bob purged alice's deleted note: %d", w.Code)
	}
	// Still there for its owner afterwards.
	decode(t, asKey(t, h, aliceKey, "GET", "/api/trash", nil), &entries)
	if len(entries) != 1 {
		t.Fatalf("alice's trashed note is gone after bob's attempts: %v", entries)
	}
	if w := asKey(t, h, aliceKey, "POST", "/api/trash/"+tid+"/restore", nil); w.Code != http.StatusOK {
		t.Fatalf("alice cannot restore her own note: %d %s", w.Code, w.Body)
	}
}

// Instance settings are levers, not content: an anonymous caller could read
// them and — worse — change them, which includes repointing this instance's
// model endpoint at a server of their choosing.
func TestSettingsAreAdministrative(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	for _, c := range []struct {
		name, key string
		want      int
	}{{"anonymous", "", http.StatusUnauthorized}, {"member", bobKey, http.StatusForbidden}} {
		if w := call(t, h, c.key, "GET", "/api/settings", nil); w.Code != c.want {
			t.Errorf("%s GET /api/settings = %d, want %d", c.name, w.Code, c.want)
		}
		if w := call(t, h, c.key, "PUT", "/api/settings",
			map[string]string{"embed_base_url": "http://attacker.example"}); w.Code != c.want {
			t.Errorf("%s PUT /api/settings = %d, want %d", c.name, w.Code, c.want)
		}
	}
	if got := s.Settings.Get("embed_base_url"); strings.Contains(got, "attacker") {
		t.Fatalf("a refused write still changed the setting: %q", got)
	}
	if w := asKey(t, h, aliceKey, "GET", "/api/settings", nil); w.Code != http.StatusOK {
		t.Fatalf("admin GET /api/settings = %d", w.Code)
	}

	// Changing the vault passphrase re-seals every encrypted note. It was the
	// one /api/vault route not wrapped adminOnly, leaning entirely on the
	// admin TOKEN — which a deployment that uses accounts instead never sets.
	// The anonymous probe cannot see this one: an uninitialized vault answers
	// before any check would, so the property worth testing is the member.
	if w := asKey(t, h, bobKey, "POST", "/api/vault/change-passphrase",
		map[string]string{"old": "a", "new": "b"}); w.Code != http.StatusForbidden {
		t.Errorf("member change-passphrase = %d, want 403", w.Code)
	}
}

// call is asKey, or do when the key is empty.
func call(t *testing.T, h http.Handler, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	if key == "" {
		return do(t, h, method, path, body)
	}
	return asKey(t, h, key, method, path, body)
}

// Surfaces that answer FROM notes without returning one: the alias map, tag
// counts, and agent memory.
//
// Each was reasoned about as if it returned "just" a name or a number, so each
// was filtered by space alone or — in the alias map's case, which took its
// request as `_` — not at all. But an alias IS the note's human title, a tag
// count tells you how many documents you cannot see carry a given label, and
// memory hands back bodies. The fixture matters here: the restricted note is
// in the COMMONS, which the other member can read, so its reader list is the
// only thing keeping him out. An earlier version of this test reused a fixture
// whose note had no alias and no tag, and passed with every filter removed.
func TestDerivedSurfacesRespectReaderLists(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.WriteNote(MemoryDir+"/severance.md",
		"# Severance\n\nALIASMARKER terms for the kestrel departure\n\n#payroll",
		map[string]any{
			"title": "Severance", "aliases": "SEVERANCEALIAS", "tags": "payroll",
			"readers": alice.ID,
		}); err != nil {
		t.Fatal(err)
	}
	// An unrestricted note in the same folder, so "bob sees nothing" cannot
	// pass by the surface being broken for everyone.
	if _, err := s.WriteNote(MemoryDir+"/standup.md",
		"# Standup\n\nkestrel notes\n\n#standup",
		map[string]any{"title": "Standup", "aliases": "STANDUPALIAS", "tags": "standup"}); err != nil {
		t.Fatal(err)
	}

	// A canvas, a journal entry and a template in her space: three listings
	// that walked the vault or the index without asking who was calling.
	if _, err := s.WriteNote("users/alice/journal-note.md", "# J\n\nx", nil); err != nil {
		t.Fatal(err)
	}
	if w := asKey(t, h, aliceKey, "PUT", "/api/canvas/users/alice/BOARDMARKER",
		map[string]any{"nodes": []any{}, "edges": []any{}}); w.Code != http.StatusOK {
		t.Fatalf("alice canvas write = %d %s", w.Code, w.Body)
	}
	if body := asKey(t, h, bobKey, "GET", "/api/canvas", nil).Body.String(); strings.Contains(body, "BOARDMARKER") {
		t.Errorf("the canvas listing leaked a board in another member's space: %s", body)
	}
	if body := asKey(t, h, aliceKey, "GET", "/api/canvas", nil).Body.String(); !strings.Contains(body, "BOARDMARKER") {
		t.Errorf("alice cannot see her own board: %s", body)
	}

	for _, path := range []string{"/api/aliases", "/api/tags", "/api/memory?q=kestrel"} {
		body := asKey(t, h, bobKey, "GET", path, nil).Body.String()
		for _, forbidden := range []string{"SEVERANCEALIAS", "ALIASMARKER", "payroll", "severance"} {
			if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
				t.Errorf("%s leaked %q to a member not on the reader list:\n%s", path, forbidden, body)
			}
		}
	}
	// Not over-blocked: the open note still shows up on each.
	for path, want := range map[string]string{
		"/api/aliases": "STANDUPALIAS", "/api/tags": "standup", "/api/memory?q=kestrel": "standup",
	} {
		if body := asKey(t, h, bobKey, "GET", path, nil).Body.String(); !strings.Contains(strings.ToLower(body), strings.ToLower(want)) {
			t.Errorf("%s hid the unrestricted note too: %s", path, body)
		}
	}
	// And alice, who is named on it, still sees hers.
	for _, path := range []string{"/api/aliases", "/api/tags", "/api/memory?q=kestrel"} {
		if body := asKey(t, h, aliceKey, "GET", path, nil).Body.String(); !strings.Contains(strings.ToLower(body), "severance") &&
			!strings.Contains(strings.ToLower(body), "payroll") {
			t.Errorf("%s hid the document from the person named on it: %s", path, body)
		}
	}
}

// Applying a template COPIES a note's body into a new note the caller owns, and
// the template path comes from the caller — so without a read check it is a
// read of any note in the vault wearing a write's clothes.
func TestTemplateApplyIsNotAReadBypass(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/offer.md", "body": "# Offer\n\nBASESALARYMARKER 210k"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}

	w := asKey(t, h, bobKey, "POST", "/api/templates/apply", map[string]any{
		"template": "users/alice/offer.md", "title": "Innocent"})
	if w.Code == http.StatusCreated {
		// If it was created, the body must not be hers — but it will be, which
		// is the point: this must be refused outright.
		var out map[string]string
		decode(t, w, &out)
		body := asKey(t, h, bobKey, "GET", "/api/notes/"+out["path"], nil).Body.String()
		t.Fatalf("bob templated another member's note into his own: %s", body)
	}
	if w.Code != http.StatusNotFound {
		t.Fatalf("template apply = %d %s, want 404 (absent, not forbidden)", w.Code, w.Body)
	}

	// A template he MAY read still works, or this is just a broken route.
	if _, err := s.WriteNote("templates/daily.md", "# {{title}}\n\nagenda", nil); err != nil {
		t.Fatal(err)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/templates/apply", map[string]any{
		"template": "templates/daily.md", "title": "Standup"}); w.Code != http.StatusCreated {
		t.Fatalf("applying a readable template = %d %s", w.Code, w.Body)
	}
}

// Routes that take a note path in the BODY rather than the URL.
//
// This is one class, not five bugs. Every access check in this package hangs
// off the path in the URL — requireRead and requireWrite are called with
// r.PathValue("path"), and the dispatcher does it once for every note route.
// A handler whose real subject arrives in the JSON body slips underneath all
// of it, and five of them did: applying a template copied any note's text into
// one the caller owned, setting a fact edited any note, memory consolidation
// rewrote any memory note, renaming moved a note INTO a space the caller
// cannot write, and linking rewrote a source note named only in the body.
//
// Each case below names the note in the body; the URL, where one is needed,
// points at something the caller legitimately owns — so a pass means the body
// path was checked, not that the request was refused for some other reason.
func TestNotePathsInRequestBodiesAreChecked(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/private.md", "body": "# Private\n\nBODYMARKER mentions kestrel here"}); w.Code != http.StatusCreated {
		t.Fatalf("alice create = %d %s", w.Code, w.Body)
	}
	if _, err := s.WriteNote(MemoryDir+"/alice-memory.md", "# Mem\n\nBODYMARKER", map[string]any{
		"readers": mustUser(t, s, "alice").ID}); err != nil {
		t.Fatal(err)
	}
	// One note per case. They shared one, and a rename that wrongly SUCCEEDED
	// moved it out from under the link case, which then failed to find it and
	// reported itself passing — one hole hiding the next.
	for _, p := range []string{"users/bob/for-rename.md", "users/bob/for-link.md"} {
		if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
			"path": p, "body": "# Mine\n\nkestrel"}); w.Code != http.StatusCreated {
			t.Fatalf("bob create %s = %d %s", p, w.Code, w.Body)
		}
	}

	cases := []struct {
		name, method, url string
		body              map[string]any
	}{
		{"template copies another member's note", "POST", "/api/templates/apply",
			map[string]any{"template": "users/alice/private.md", "title": "Copy"}},
		{"fact edits another member's note", "POST", "/api/facts",
			map[string]any{"note": "users/alice/private.md", "key": "k", "value": "v"}},
		{"consolidate rewrites another member's memory", "POST", "/api/memory/consolidate",
			map[string]any{"path": MemoryDir + "/alice-memory.md"}},
		{"rename moves a note into a space the caller cannot write", "POST",
			"/api/notes/users/bob/for-rename.md/rename",
			map[string]any{"new_path": "users/alice/planted.md"}},
		{"link rewrites a source note named in the body", "POST",
			"/api/notes/users/bob/for-link.md/link",
			map[string]any{"source": "users/alice/private.md", "name": "kestrel"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := asKey(t, h, bobKey, c.method, c.url, c.body)
			if w.Code >= 200 && w.Code < 300 {
				t.Errorf("%s %s succeeded (%d): %s", c.method, c.url, w.Code, w.Body)
			}
		})
	}

	// Nothing of alice's moved, changed, or was copied out.
	if w := asKey(t, h, aliceKey, "GET", "/api/notes/users/alice/private.md", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "BODYMARKER") {
		t.Fatalf("alice's note was altered or lost: %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, aliceKey, "GET", "/api/notes/users/alice/planted.md", nil); w.Code == http.StatusOK {
		t.Error("a note was planted in alice's space by rename")
	}
	if body := asKey(t, h, bobKey, "GET", "/api/notes", nil).Body.String(); strings.Contains(body, "BODYMARKER") {
		t.Errorf("alice's text reached bob's own notes: %s", body)
	}
}

func mustUser(t *testing.T, s *Server, name string) auth.User {
	t.Helper()
	u, err := s.Auth.ByName(name)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The note-action routes — encrypt, decrypt, pin, delete, restore a version.
//
// Each is gated by the dispatcher rather than by anything in the handler, which
// is a perfectly good place for the check to live and a bad thing to take on
// faith: a handler sweep flags all of them as unguarded, and the only way to
// tell a safe indirection from a hole is to drive the route. If someone later
// registers one of these directly, the dispatcher's check is silently gone and
// this is what says so.
func TestNoteActionRoutesAreGatedByTheDispatcher(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	const path = "users/alice/ledger.md"
	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": path, "body": "# Ledger\n\nACTIONMARKER"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}

	for _, c := range []struct{ method, url string }{
		{"POST", "/api/notes/" + path + "/pin"},
		{"POST", "/api/notes/" + path + "/encrypt"},
		{"POST", "/api/notes/" + path + "/decrypt"},
		{"POST", "/api/notes/" + path + "/rename"},
		{"POST", "/api/notes/" + path + "/duplicate"},
		{"POST", "/api/notes/" + path + "/history/1/restore"},
		{"DELETE", "/api/notes/" + path},
		{"GET", "/api/notes/" + path + "/history"},
		{"GET", "/api/notes/" + path + "/export.html"},
		{"GET", "/api/crdt/doc/" + path},
	} {
		t.Run(c.method+" "+c.url, func(t *testing.T) {
			if w := asKey(t, h, bobKey, c.method, c.url, map[string]any{"new_path": "x.md"}); w.Code >= 200 && w.Code < 300 {
				t.Errorf("%s %s succeeded for a member who cannot read it: %d %s",
					c.method, c.url, w.Code, w.Body)
			}
		})
	}
	// Untouched and still hers.
	w := asKey(t, h, aliceKey, "GET", "/api/notes/"+path, nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ACTIONMARKER") {
		t.Fatalf("alice's note changed under her: %d %s", w.Code, w.Body)
	}
}

// The rest of what a handler sweep turned up: routes that write to a path the
// caller chooses indirectly — a memory topic, a template name, a tag — and the
// CRDT pair, where the document IS the note's text.
//
// renameTag is the one worth naming. It rewrites the BODY of every note
// carrying a tag, it was registered with no principal check at all, and tags
// cross spaces by design. One request re-wrote the whole vault on behalf of
// anyone who could reach the port.
func TestIndirectWriteTargetsAreChecked(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/tagged.md",
		"body": "# Tagged\n\nTAGMARKER body\n\n#shared"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
		"path": "users/bob/tagged.md", "body": "# Mine\n\nmine\n\n#shared"}); w.Code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", w.Code, w.Body)
	}

	// A tag rename touches only what the caller may write, and says how much
	// it skipped rather than pretending it did everything.
	w := asKey(t, h, bobKey, "POST", "/api/tags/rename",
		map[string]any{"old": "shared", "new": "renamed"})
	if w.Code != http.StatusOK {
		t.Fatalf("tag rename = %d %s", w.Code, w.Body)
	}
	var out struct{ Notes, Skipped int }
	decode(t, w, &out)
	if out.Notes != 1 || out.Skipped != 1 {
		t.Errorf("tag rename touched %d notes and skipped %d; want 1 and 1", out.Notes, out.Skipped)
	}
	body := asKey(t, h, aliceKey, "GET", "/api/notes/users/alice/tagged.md", nil).Body.String()
	if !strings.Contains(body, "#shared") || strings.Contains(body, "#renamed") {
		t.Errorf("alice's note was rewritten by another member's tag rename: %s", body)
	}
	if w := do(t, h, "POST", "/api/tags/rename",
		map[string]any{"old": "shared", "new": "x"}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous tag rename = %d, want 401", w.Code)
	}

	// The CRDT pair: reading a document is reading the note, merging is writing
	// it, and the merge path arrives in the body.
	if w := asKey(t, h, bobKey, "GET", "/api/crdt/doc/users/alice/tagged.md", nil); w.Code < 400 {
		t.Errorf("crdt doc read of another member's note = %d", w.Code)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/crdt/merge", map[string]any{
		"path": "users/alice/tagged.md", "doc": map[string]any{}}); w.Code < 400 {
		t.Errorf("crdt merge into another member's note = %d", w.Code)
	}
	if !strings.Contains(asKey(t, h, aliceKey, "GET", "/api/notes/users/alice/tagged.md", nil).Body.String(), "TAGMARKER") {
		t.Error("alice's note was altered through the CRDT routes")
	}

	// And the write routes whose destination the caller names indirectly.
	for _, c := range []struct {
		name, url string
		body      map[string]any
	}{
		{"memory topic", "/api/memory", map[string]any{"topic": "x", "text": "y"}},
		{"template name", "/api/templates", map[string]any{"name": "x", "body": "y"}},
		{"capture", "/api/capture", map[string]any{"text": "y", "title": "x"}},
	} {
		if w := do(t, h, "POST", c.url, c.body); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", c.name, w.Code)
		}
	}
}

// A leak sweep that does not depend on remembering which surfaces exist.
//
// Every test above names the routes it checks, so each is only as complete as
// the list someone typed — and the holes found today were, without exception,
// on routes nobody had thought to add to a list. This drives every GET route
// the mux registers, as a member who must not see one particular note, and
// fails if a marker from that note appears in any response body.
//
// It cannot prove a surface is safe (a route needing parameters may answer
// nothing at all), so it does not replace the targeted tests. What it does is
// make the DEFAULT for a newly registered read route "checked by something"
// rather than "checked if remembered".
func TestNoGetRouteLeaksARestrictedNote(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin")
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")
	alice := mustUser(t, s, "alice")

	// Two restricted notes: one kept away by SPACE, one in the commons kept
	// away only by its READER LIST. They fail independently.
	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/spacesecret.md",
		"body": "# Space Secret\n\nSPACELEAKMARKER kestrel\n\n#spacetag"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	if _, err := s.WriteNote("commons-restricted.md",
		"# Commons Restricted\n\nACLLEAKMARKER kestrel\n\n#acltag",
		map[string]any{"title": "ACLLEAKTITLE", "aliases": "ACLLEAKALIAS",
			"tags": "acltag", "readers": alice.ID}); err != nil {
		t.Fatal(err)
	}
	// Something bob CAN see, so a route answering nothing at all is visible as
	// a gap in this test rather than as a pass.
	if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
		"path": "users/bob/ok.md", "body": "# OK\n\nkestrel visible"}); w.Code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", w.Code, w.Body)
	}

	markers := []string{"SPACELEAKMARKER", "ACLLEAKMARKER", "ACLLEAKTITLE",
		"ACLLEAKALIAS", "spacesecret", "commons-restricted", "acltag", "spacetag"}

	for _, route := range registeredRoutes(t) {
		method, pattern, ok := strings.Cut(route, " ")
		if !ok || method != "GET" {
			continue
		}
		if pattern == "/metrics" { // route classes and counts, never content
			continue
		}
		path := fillWildcards(pattern)
		for _, q := range []string{"", "?q=kestrel&k=20", "?q=kestrel&full=true", "?include_private=true"} {
			w := asKey(t, h, bobKey, "GET", path+q, nil)
			body := w.Body.String()
			for _, m := range markers {
				if strings.Contains(body, m) {
					t.Errorf("GET %s%s leaked %q to a member who may not read it:\n%s",
						path, q, m, truncate(body, 400))
				}
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
