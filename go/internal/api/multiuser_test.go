package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// End-to-end multi-user behaviour, over the real HTTP surface.
//
// The property under test throughout: nothing changes for a deployment with no
// accounts, and once accounts exist, one member cannot reach another's notes
// through ANY surface — not the note route, not listing, not search, not
// retrieval, not the agent briefing.

// requestFor builds a JSON request the way do() does, so tests can add
// credentials to it.
func requestFor(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// asKey performs a request carrying an API key.
func asKey(t *testing.T, h http.Handler, key, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	req := requestFor(t, method, path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// makeUser creates an account. The first one needs no credentials — that is
// the act that turns multi-user on — and every later one is an administrator's
// doing, so the admin's key is passed in.
func makeUser(t *testing.T, s *Server, h http.Handler, adminKey, name, role string) string {
	t.Helper()
	body := map[string]any{
		"name": name, "display": name, "password": "correct horse battery", "role": role}
	var w *httptest.ResponseRecorder
	if adminKey == "" {
		w = do(t, h, "POST", "/api/users", body)
	} else {
		w = asKey(t, h, adminKey, "POST", "/api/users", body)
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("creating %s: %d %s", name, w.Code, w.Body)
	}
	u, err := s.Auth.ByName(name)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := s.Auth.CreateAPIKey(u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestSingleUserDeploymentIsUnchanged(t *testing.T) {
	_, h := testServer(t)
	// no accounts: everything works with no credentials at all
	if w := do(t, h, "POST", "/api/notes", map[string]any{
		"path": "note.md", "body": "# Note\n\nbody"}); w.Code != http.StatusCreated {
		t.Fatalf("create = %d", w.Code)
	}
	if w := do(t, h, "GET", "/api/notes/note.md", nil); w.Code != http.StatusOK {
		t.Fatalf("read = %d", w.Code)
	}
	var me map[string]any
	decode(t, do(t, h, "GET", "/api/me", nil), &me)
	if me["multi_user"] != false || me["anonymous"] != false || me["admin"] != true {
		t.Fatalf("/api/me on a single-user instance = %v", me)
	}
	// and the credential vault is still reachable without signing in
	if w := do(t, h, "GET", "/api/vault/status", nil); w.Code != http.StatusOK {
		t.Fatalf("vault status = %d", w.Code)
	}
}

func TestMembersCannotReachEachOthersNotes(t *testing.T) {
	s, h := testServer(t)
	aliceKey := makeUser(t, s, h, "", "alice", "admin") // first account is the admin
	bobKey := makeUser(t, s, h, aliceKey, "bob", "member")

	// Alice writes in her personal space; Bob writes in his.
	if w := asKey(t, h, aliceKey, "POST", "/api/notes", map[string]any{
		"path": "users/alice/diary.md", "body": "# Diary\n\nkestrel thoughts"}); w.Code != http.StatusCreated {
		t.Fatalf("alice create = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "POST", "/api/notes", map[string]any{
		"path": "users/bob/diary.md", "body": "# Bob diary\n\nkestrel plans"}); w.Code != http.StatusCreated {
		t.Fatalf("bob create = %d %s", w.Code, w.Body)
	}

	// Bob cannot read Alice's note, and is told it does not exist rather than
	// that he may not see it.
	w := asKey(t, h, bobKey, "GET", "/api/notes/users/alice/diary.md", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("bob reading alice's note = %d %s", w.Code, w.Body)
	}
	// nor write into her space
	if w := asKey(t, h, bobKey, "PUT", "/api/notes/users/alice/diary.md",
		map[string]any{"body": "# Overwritten"}); w.Code == http.StatusOK {
		t.Fatal("bob overwrote alice's note")
	}
	// nor delete it
	if w := asKey(t, h, bobKey, "DELETE", "/api/notes/users/alice/diary.md", nil); w.Code == http.StatusOK {
		t.Fatal("bob deleted alice's note")
	}

	// Listing, search, retrieval and the briefing all agree.
	for _, path := range []string{
		"/api/notes", "/api/search?q=kestrel", "/api/retrieve?q=kestrel&k=10",
		"/api/briefing", "/api/graph", "/api/tasks", "/api/complete?q=diary",
	} {
		body := asKey(t, h, bobKey, "GET", path, nil).Body.String()
		if strings.Contains(body, "users/alice") {
			t.Errorf("%s leaked alice's note to bob: %s", path, body)
		}
	}

	// Alice still sees her own, and Bob sees his.
	if !strings.Contains(asKey(t, h, aliceKey, "GET", "/api/notes", nil).Body.String(),
		"users/alice/diary.md") {
		t.Error("alice cannot see her own note")
	}
	if !strings.Contains(asKey(t, h, bobKey, "GET", "/api/notes", nil).Body.String(),
		"users/bob/diary.md") {
		t.Error("bob cannot see his own note")
	}
}

func TestSharedSpacesAndReadOnlyMembership(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	w := asKey(t, h, adminKey, "POST", "/api/spaces",
		map[string]any{"name": "Engineering", "prefix": "team/eng"})
	if w.Code != http.StatusCreated {
		t.Fatalf("create space = %d %s", w.Code, w.Body)
	}
	var space map[string]any
	decode(t, w, &space)
	if w := asKey(t, h, adminKey, "POST", "/api/notes", map[string]any{
		"path": "team/eng/runbook.md", "body": "# Runbook\n\nrollback steps"}); w.Code != http.StatusCreated {
		t.Fatalf("admin create in space = %d %s", w.Code, w.Body)
	}

	// Not a member yet.
	if w := asKey(t, h, bobKey, "GET", "/api/notes/team/eng/runbook.md", nil); w.Code != http.StatusNotFound {
		t.Fatalf("non-member read = %d", w.Code)
	}
	// Added as a reader.
	if w := asKey(t, h, adminKey, "POST", "/api/spaces/"+space["id"].(string)+"/members",
		map[string]any{"user": "bob", "role": "reader"}); w.Code != http.StatusOK {
		t.Fatalf("add member = %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, "GET", "/api/notes/team/eng/runbook.md", nil); w.Code != http.StatusOK {
		t.Fatalf("member read = %d %s", w.Code, w.Body)
	}
	// A reader may not write, and is told so rather than told it is missing.
	w = asKey(t, h, bobKey, "PUT", "/api/notes/team/eng/runbook.md", map[string]any{"body": "# no"})
	if w.Code != http.StatusForbidden {
		t.Fatalf("reader write = %d %s", w.Code, w.Body)
	}
	// Promoted to writer.
	if w := asKey(t, h, adminKey, "POST", "/api/spaces/"+space["id"].(string)+"/members",
		map[string]any{"user": "bob", "role": "writer"}); w.Code != http.StatusOK {
		t.Fatal("promote failed")
	}
	if w := asKey(t, h, bobKey, "PUT", "/api/notes/team/eng/runbook.md",
		map[string]any{"body": "# Runbook\n\nedited by bob"}); w.Code != http.StatusOK {
		t.Fatalf("writer write = %d %s", w.Code, w.Body)
	}
}

func TestAnonymousCallersSeeNothingOnceAccountsExist(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	asKey(t, h, adminKey, "POST", "/api/notes", map[string]any{
		"path": "secret.md", "body": "# Secret\n\nkestrel"})

	for _, path := range []string{"/api/notes", "/api/search?q=kestrel",
		"/api/retrieve?q=kestrel&k=5", "/api/briefing", "/api/tags"} {
		body := do(t, h, "GET", path, nil).Body.String()
		if strings.Contains(body, "secret.md") || strings.Contains(body, "kestrel") {
			t.Errorf("anonymous caller saw %s: %s", path, body)
		}
	}
	if w := do(t, h, "GET", "/api/notes/secret.md", nil); w.Code != http.StatusNotFound {
		t.Errorf("anonymous note read = %d", w.Code)
	}
	if w := do(t, h, "POST", "/api/notes", map[string]any{"path": "x.md", "body": "x"}); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous write = %d, want 401", w.Code)
	}
	// and administration is closed
	for _, path := range []string{"/api/users", "/api/secrets", "/api/audit"} {
		if w := do(t, h, "GET", path, nil); w.Code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", path, w.Code)
		}
	}
}

func TestMembersCannotAdministerTheInstance(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/users", nil},
		{"POST", "/api/users", map[string]any{"name": "mallory", "password": "correct horse battery"}},
		{"GET", "/api/secrets", nil},
		{"GET", "/api/audit", nil},
		{"POST", "/api/spaces", map[string]any{"name": "x", "prefix": "x"}},
		{"POST", "/api/reindex", nil},
	} {
		w := asKey(t, h, bobKey, c.method, c.path, c.body)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s as a member = %d, want 403", c.method, c.path, w.Code)
		}
	}
}

func TestLoginIssuesASessionAndLogoutEndsIt(t *testing.T) {
	s, h := testServer(t)
	makeUser(t, s, h, "", "alice", "admin")

	w := do(t, h, "POST", "/api/auth/login",
		map[string]any{"name": "alice", "password": "correct horse battery"})
	if w.Code != http.StatusOK {
		t.Fatalf("login = %d %s", w.Code, w.Body)
	}
	cookie := w.Result().Cookies()
	if len(cookie) == 0 || cookie[0].Value == "" {
		t.Fatal("login set no session cookie")
	}
	if !cookie[0].HttpOnly {
		t.Error("the session cookie is readable from JavaScript")
	}

	withCookie := func(method, path string) *httptest.ResponseRecorder {
		req := requestFor(t, method, path, nil)
		req.AddCookie(cookie[0])
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	var me map[string]any
	if err := json.Unmarshal(withCookie("GET", "/api/me").Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["name"] != "alice" || me["admin"] != true {
		t.Fatalf("/api/me with a session = %v", me)
	}

	if w := do(t, h, "POST", "/api/auth/login",
		map[string]any{"name": "alice", "password": "wrong"}); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong password = %d", w.Code)
	}
	if got := withCookie("POST", "/api/auth/logout").Code; got != http.StatusOK {
		t.Fatalf("logout = %d", got)
	}
	if err := json.Unmarshal(withCookie("GET", "/api/me").Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me["anonymous"] != true {
		t.Fatalf("session survived logout: %v", me)
	}
}

// End-to-end: a pulled document restricted to one person, over HTTP.
//
// This is the gap the README named as the biggest thing an enterprise search
// product does that this did not. It is narrower than a full ACL mirror — the
// source's identities have to be mapped to accounts explicitly — but the
// property it buys is the one that matters: a document one colleague may read
// is invisible to another, through every surface.
func TestAPulledDocumentCanBeRestrictedToOnePerson(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}

	// The administrator maps Slack's idea of alice to this instance's.
	if w := asKey(t, h, adminKey, "POST", "/api/identities", map[string]any{
		"source": "slack", "external": "U_ALICE", "user": "alice"}); w.Code != http.StatusCreated {
		t.Fatalf("mapping an identity = %d %s", w.Code, w.Body)
	}

	// A connector writes a note carrying that reader list. (Written directly:
	// the runner's own resolution is covered in the connectors package.)
	if _, err := s.WriteNote("connectors/slack/private-thread.md",
		"# Private thread\n\nthe kestrel decision", map[string]any{
			"title": "Private thread", "source": "slack",
			"readers": alice.ID,
		}); err != nil {
		t.Fatal(err)
	}

	// Alice sees it everywhere; Bob nowhere.
	for _, path := range []string{
		"/api/search?q=kestrel", "/api/retrieve?q=kestrel&k=10", "/api/notes",
	} {
		if body := asKey(t, h, aliceKey, "GET", path, nil).Body.String(); !strings.Contains(body, "private-thread") {
			t.Errorf("alice cannot see a document she is named on: %s -> %s", path, body)
		}
		if body := asKey(t, h, bobKey, "GET", path, nil).Body.String(); strings.Contains(body, "kestrel decision") {
			t.Errorf("%s leaked a restricted document to bob: %s", path, body)
		}
	}
	// Retrieval is the surface that matters most: the chunk text must not
	// appear even though the note is in a space bob CAN read.
	if body := asKey(t, h, bobKey, "GET", "/api/retrieve?q=kestrel&k=10", nil).Body.String(); strings.Contains(body, "kestrel decision") {
		t.Error("retrieval returned a restricted chunk")
	}
	// And a member cannot grant themselves access by mapping an identity.
	if w := asKey(t, h, bobKey, "POST", "/api/identities", map[string]any{
		"source": "slack", "external": "U_ALICE", "user": "bob"}); w.Code != http.StatusForbidden {
		t.Errorf("a member mapped an identity: %d", w.Code)
	}
}
