package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

// Deny-probe leakage suite.
//
// The question this file answers is not "does the handler check a principal"
// (route_audit_test.go and multiuser_test.go do that) but "does ANY byte that
// belongs to a private owner reach a caller who may not see it, through ANY
// read surface, in any shape". Each owned item carries a distinctive canary
// string. Every probe runs as:
//
//   - bob       — an ordinary member with no grant on alice's spaces
//   - anon      — no credentials on a deployment that has accounts
//   - bob+agent — bob's key with an X-Grimoire-Agent header (an agent identity
//                 that holds no grant of its own)
//   - anon+agent
//
// and the response body must contain no canary. A second layer checks the
// things a body can leak without spelling a canary out: status codes on by-id
// reads (404/403, never a 200 with an empty body that confirms existence),
// count fields (totals, facets, degree, tag and entity counts) that must not
// differ between a query that matches hidden content and one that matches
// nothing, and the MCP dispatcher, which must refuse the same things.
//
// Coverage is enforced, not hoped for: every registered route must have an
// explicit probe in lpProbes, an entry in lpExempt with a reason, or be an
// admin-class route (which TestLeakprobeAdminRoutesRefuseMembers drives). A
// generic sweep also drives every registered route with concrete stand-in ids,
// so a route nobody remembered to list still gets its canary check.

// lpPublicOK is the one canary allowed to leave the box anonymously: the note
// the operator chose to publish. It may appear ONLY on the published routes.
const lpPublicOK = "LKC-public-ok"

// lpCommonsShared are canaries that live in the commons. The commons is
// readable by every account by design (auth.PrincipalFor: "everyone reads and
// writes the commons"), and agent memory is written to memory/<topic>.md, which
// is in the commons. So a MEMBER may see these; an anonymous caller may not.
// They are still asserted hidden from every anonymous identity.
var lpCommonsShared = map[string]bool{
	"memory": true, "agent-mem": true, "challenged": true, "disputed": true,
	// Uploads carry no owner: documentImport writes documents/<name>.md in the
	// commons, so a member can read what another member uploaded.
	"imported-doc": true,
	// "private" is a retrieval-exclusion flag in this codebase, not an owner
	// boundary: listings and direct reads by a member of the space show it, and
	// only automatic/agent retrieval and the read surface exclude it. Recorded
	// as a design finding in docs/SECURITY-LEAKPROBE.md.
	"shared-private": true,
}

// lpCanaries are the owned secrets. Labels are stable so failure messages say
// which surface leaked which item.
var lpCanaries = map[string]string{
	"shared-private": "LKC-sharedprivate-9a4d",
	"diary":          "LKC-diary-7f3a",
	"private":        "LKC-private-b81e",
	"published":      lpPublicOK,
	"pubprivate":     "LKC-pubprivate-c402",
	"old-version":    "LKC-oldversion-d911",
	"trashed":        "LKC-trashed-e5a7",
	"memory":         "LKC-memory-f0c8",
	"agent-mem":      "LKC-agentmem-a19d",
	"challenged":     "LKC-challenged-2b6f",
	"disputed":       "LKC-disputed-3c70",
	"bank-fact":      "LKC-bankfact-4d81",
	"bank-model":     "LKC-bankmodel-5e92",
	"bank-direct":    "LKC-bankdirect-6fa3",
	"imported-doc":   "LKC-importeddoc-70b4",
	"graph-entity":   "LKC-graphentity-81c5",
}

// lpNoMatch is a control query that matches nothing in any fixture.
const lpNoMatch = "zqxnomatchqzx"

// lpShadow is a control with the same shape as a canary and no content behind
// it. A canary query and its shadow must produce the same counts for a
// non-owner: the only thing allowed to differ is what the caller may see.
const lpShadow = "LKC-shadow-0000"

type lpWho struct {
	name  string
	key   string
	agent string
}

type lpWorld struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	srv   *httptest.Server
	admin string
	alice string
	bob   string
	// ids captured from seeding responses, for by-id probes
	ids map[string]string
}

// who lists the identities every probe runs as.
func (w *lpWorld) who() []lpWho {
	return []lpWho{
		{name: "bob", key: w.bob},
		{name: "anon"},
		{name: "bob+agent", key: w.bob, agent: "rogue-agent"},
		{name: "anon+agent", agent: "rogue-agent"},
	}
}

// do performs one request as an identity, in-process.
func (w *lpWorld) do(who lpWho, method, path string, body any) (int, string) {
	w.t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			w.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if who.key != "" {
		req.Header.Set("Authorization", "Bearer "+who.key)
	}
	if who.agent != "" {
		req.Header.Set("X-Grimoire-Agent", who.agent)
	}
	rec := httptest.NewRecorder()
	w.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// as performs a request as alice, failing loudly if seeding does not land.
func (w *lpWorld) as(key, method, path string, body any) (int, string) {
	w.t.Helper()
	return w.do(lpWho{key: key}, method, path, body)
}

func (w *lpWorld) mustOK(what string, code int, body string, want ...int) {
	w.t.Helper()
	for _, c := range want {
		if code == c {
			return
		}
	}
	w.t.Fatalf("seeding %s: status %d, want one of %v: %s", what, code, want, body)
}

// seedLeakWorld builds two members and an admin, and seeds every canary as
// alice. Seeding asserts its own success so a probe can never pass against an
// empty fixture.
func seedLeakWorld(t *testing.T) *lpWorld {
	t.Helper()
	return seedLeakWorldWith(t, true)
}

// seedLeakWorldWith seeds the fixture. With owned=false the same commons
// content is written but alice's owner-only items (her personal notes and her
// bank) are not, which is the control world for the differential count test.
func seedLeakWorldWith(t *testing.T, owned bool) *lpWorld {
	t.Helper()
	s, h := testServer(t)
	if err := s.Settings.Update(map[string]string{PublishSetting: "true"}); err != nil {
		t.Fatal(err)
	}
	w := &lpWorld{t: t, s: s, h: h, ids: map[string]string{}}
	w.srv = httptest.NewServer(h)
	t.Cleanup(w.srv.Close)

	w.admin = makeUser(t, s, h, "", "root", "admin")
	w.alice = makeUser(t, s, h, w.admin, "alice", "member")
	w.bob = makeUser(t, s, h, w.admin, "bob", "member")
	c := lpCanaries
	var code int
	var body string
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}

	if owned {
		// Alice's personal space is users/alice/. Bob has no membership in it.
		// Notes.
		code, body := w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/diary.md", "body": "# Diary\n\n" + c["diary"] + " kestrel"})
		w.mustOK("diary", code, body, 201)
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/private.md", "body": "# Private\n\n" + c["private"],
			"frontmatter": map[string]any{"private": true}})
		w.mustOK("private note", code, body, 201)
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/published.md", "body": "# Published\n\n" + c["published"],
			"frontmatter": map[string]any{"publish": true}})
		w.mustOK("published note", code, body, 201)
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/pubprivate.md", "body": "# PubPrivate\n\n" + c["pubprivate"],
			"frontmatter": map[string]any{"publish": true, "private": true}})
		w.mustOK("publish+private note", code, body, 201)
		// Superseded version: the first body is replaced, and history keeps it.
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/versioned.md", "body": "# V\n\n" + c["old-version"]})
		w.mustOK("versioned note", code, body, 201)
		code, body = w.as(w.alice, "PUT", "/api/notes/users/alice/versioned.md", map[string]any{
			"body": "# V\n\nreplaced text"})
		w.mustOK("versioned edit", code, body, 200)
		// Trash: created, then deleted.
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/gone.md", "body": "# Gone\n\n" + c["trashed"]})
		w.mustOK("trashed note", code, body, 201)
		code, body = w.as(w.alice, "DELETE", "/api/notes/users/alice/gone.md", nil)
		w.mustOK("trash delete", code, body, 200)

	}
	// Agent memory. remember() writes to memory/<topic>.md, which is commons,
	// so these are readable by members and must stay hidden from anonymous
	// callers (see lpCommonsShared). Session and agent are the scoping keys.
	code, body = w.as(w.alice, "POST", "/api/memory", map[string]any{
		"text": c["memory"] + " the vendor contract renews in March", "topic": "vendor",
		"agent": "alice-agent", "session": "sess-alice", "task": "contracts"})
	w.mustOK("memory", code, body, 201, 200)
	code, body = w.as(w.alice, "POST", "/api/memory", map[string]any{
		"text": c["agent-mem"] + " alice prefers the blue team", "topic": "prefs",
		"agent": "alice-agent", "session": "sess-alice"})
	w.mustOK("agent memory", code, body, 201, 200)
	code, body = w.as(w.alice, "POST", "/api/memory", map[string]any{
		"text": c["challenged"] + " the server is in rack B", "topic": "infra",
		"agent": "alice-agent"})
	w.mustOK("challenged memory", code, body, 201, 200)
	code, body = w.as(w.alice, "POST", "/api/memory", map[string]any{
		"text": c["disputed"] + " the server is in rack C", "topic": "infra",
		"agent": "alice-agent"})
	w.mustOK("disputed memory", code, body, 201, 200)

	// An imported document (a pulled file, so it arrives untrusted).
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "contract.md")
	if err != nil {
		t.Fatal(err)
	}
	fw.Write([]byte("# Contract\n\n" + c["imported-doc"] + " signed by the vendor\n"))
	mw.Close()
	req := httptest.NewRequest("POST", "/api/documents/import", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+w.alice)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	w.mustOK("document import", rec.Code, rec.Body.String(), 200, 201)

	// A private note in a SHARED space. Bob is a member of the commons, so the
	// only thing keeping this from him is the private flag.
	code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
		"path": "team/alice-private.md", "body": "# Team private\n\n" + c["shared-private"],
		"frontmatter": map[string]any{"private": true}})
	w.mustOK("shared private note", code, body, 201)

	if owned {
		// Bank: a private bank whose folder sits in a space only alice writes.
		// Bob is not a member, so every bank route must answer as if it is absent.
		code, body = w.as(w.admin, "POST", "/api/spaces", map[string]any{
			"Name": "alice bank", "Prefix": "banks/alicebank"})
		w.mustOK("bank space", code, body, 201)
		var sp struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(body), &sp); err != nil || sp.ID == "" {
			t.Fatalf("bank space id: %v %s", err, body)
		}
		code, body = w.as(w.admin, "POST", "/api/spaces/"+sp.ID+"/members", map[string]any{
			"User": alice.ID, "Role": "writer"})
		w.mustOK("bank space member", code, body, 201, 200, 204)
		code, body = w.as(w.alice, "POST", "/api/banks", map[string]any{
			"bank_id": "alicebank", "name": "Alice", "mission": "private planning"})
		w.mustOK("bank", code, body, 201)
		code, body = w.as(w.alice, "POST", "/api/banks/alicebank/memories", map[string]any{
			"items": []map[string]any{{"content": c["bank-fact"] + " Priya works at Initech"}}})
		w.mustOK("bank memory", code, body, 200, 201)
		code, body = w.as(w.alice, "POST", "/api/banks/alicebank/mental-models", map[string]any{
			"id": "priya", "name": "Priya", "question": "where does priya work",
			"content": c["bank-model"] + " Priya is the contact at Initech"})
		w.mustOK("mental model", code, body, 200, 201)
		code, body = w.as(w.alice, "POST", "/api/banks/alicebank/directives", map[string]any{
			"name": "rule", "text": c["bank-direct"] + " never share the Initech contract"})
		w.mustOK("directive", code, body, 200, 201)

		// A graph entity that only alice's notes mention.
		code, body = w.as(w.alice, "POST", "/api/notes", map[string]any{
			"path": "users/alice/graph.md", "body": "# Graph\n\n[[" + c["graph-entity"] + "]] links to [[Diary]]"})
		w.mustOK("graph note", code, body, 201)

	}
	// Commons content bob IS allowed to see, so a probe can tell "hidden" from
	// "nothing in the vault matches".
	code, body = w.as(w.bob, "POST", "/api/notes", map[string]any{
		"path": "bob/hello.md", "body": "# Hello\n\nkestrel is a bird"})
	w.mustOK("bob note", code, body, 201)

	// Ids the by-id probes address, taken from alice's own listings so the probe
	// targets exist. A missing id falls back to a stand-in that still exercises
	// the handler's lookup.
	w.ids["memory"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/memories"))
	w.ids["document"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/documents"))
	w.ids["observation"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/observations"))
	w.ids["operation"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/operations"))
	w.ids["webhook"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/webhooks"))
	w.ids["trash"] = lpFirstID(w.asBody(w.alice, "/api/trash"))
	w.ids["entity"] = lpFirstID(w.asBody(w.alice, "/api/banks/alicebank/entities"))
	if owned && w.ids["memory"] == "" {
		t.Fatalf("fixture sanity: alice has no bank memory id to probe")
	}

	// Sanity: alice can see her own memory, or the probes below prove nothing.
	if code, body := w.as(w.alice, "GET", "/api/memory?q=vendor", nil); code != 200 || !strings.Contains(body, c["memory"]) {
		t.Fatalf("fixture sanity: alice cannot recall her own memory (%d): %s", code, body)
	}
	if code, body := w.as(w.alice, "GET", "/api/banks/alicebank/memories?q=Priya", nil); owned && (code != 200 || !strings.Contains(body, c["bank-fact"])) {
		t.Fatalf("fixture sanity: alice cannot list her own bank (%d): %s", code, body)
	}
	return w
}

// asBody is a GET as alice that returns the body, for capturing ids.
func (w *lpWorld) asBody(key, path string) string {
	_, body := w.as(key, "GET", path, nil)
	return body
}

var lpIDRE = regexp.MustCompile(`"id":\s*"([^"]+)"`)

// lpFirstID returns the first "id" field in a JSON body, or "".
func lpFirstID(body string) string {
	if m := lpIDRE.FindStringSubmatch(body); m != nil {
		return m[1]
	}
	return ""
}

// lpKind says what a probe's correct answer looks like.
type lpKind int

const (
	// lpByID: a single object addressed by id or path. A non-owner must get
	// 401, 403 or 404 — never 200, even with an empty body.
	lpByID lpKind = iota
	// lpList: a collection or search. 200 is fine, provided nothing hidden is
	// in the body and the counts do not move with the hidden matches.
	lpList
	// lpWrite: a mutation. Must be refused for a non-owner.
	lpWrite
)

type lpProbe struct {
	route  string // "METHOD /pattern", as routeAccess keys it
	path   string // concrete path
	method string // overrides the route's method when set (rare)
	body   any
	kind   lpKind
	// control is the same request with a query that matches nothing; list
	// responses must match it on every count field.
	control string
}

// lpProbes is the explicit deny-probe table: one or more entries for every
// content-bearing route. A route not present here must be in lpExempt.
func lpProbes(w *lpWorld) []lpProbe {
	c := lpCanaries
	q := func(s string) string { return "?q=" + strings.ReplaceAll(s, " ", "+") }
	return []lpProbe{
		// --- notes: by id and by path ---
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/diary.md", kind: lpByID},
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/private.md", kind: lpByID},
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/versioned.md/history", kind: lpByID},
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/versioned.md/history/1", kind: lpByID},
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/diary.md/unlinked", kind: lpByID},
		{route: "GET /api/notes/{path...}", path: "/api/notes/users/alice/diary.md/export.html", kind: lpByID},
		{route: "GET /api/notes", path: "/api/notes", kind: lpList},
		{route: "GET /api/notes/random", path: "/api/notes/random", kind: lpList},
		{route: "PUT /api/notes/{path...}", path: "/api/notes/users/alice/diary.md", method: "PUT", body: map[string]any{"body": "overwritten"}, kind: lpWrite},
		{route: "POST /api/notes/{path...}", path: "/api/notes/users/alice/diary.md/pin", method: "POST", kind: lpWrite},
		{route: "DELETE /api/notes/{path...}", path: "/api/notes/users/alice/diary.md", method: "DELETE", kind: lpWrite},
		{route: "POST /api/notes", path: "/api/notes", body: map[string]any{"path": "users/alice/diary.md", "body": "x"}, kind: lpWrite},
		{route: "GET /read", path: "/read", kind: lpList},
		{route: "GET /read/{path...}", path: "/read/users/alice/diary", kind: lpByID},
		{route: "GET /notes/{path...}", path: "/notes/users/alice/diary.md", kind: lpByID},
		{route: "GET /api/file/{path...}", path: "/api/file/users/alice/diary.md", kind: lpByID},
		{route: "GET /api/crdt/doc/{path...}", path: "/api/crdt/doc/users/alice/diary.md", kind: lpByID},
		{route: "POST /api/crdt/merge", path: "/api/crdt/merge", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "GET /api/canvas", path: "/api/canvas", kind: lpList},
		{route: "GET /api/canvas/{path...}", path: "/api/canvas/users/alice/diary.md", kind: lpByID},
		{route: "PUT /api/canvas/{path...}", path: "/api/canvas/users/alice/diary.md", method: "PUT", body: map[string]any{}, kind: lpWrite},
		{route: "DELETE /api/canvas/{path...}", path: "/api/canvas/users/alice/diary.md", method: "DELETE", kind: lpWrite},
		{route: "POST /api/canvas", path: "/api/canvas", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},

		// --- search and retrieval: counts must not move with hidden matches ---
		{route: "GET /api/search", path: "/api/search" + q(c["diary"]), kind: lpList, control: "/api/search" + q(lpShadow)},
		{route: "GET /api/retrieve", path: "/api/retrieve" + q(c["diary"]) + "&k=10", kind: lpList, control: "/api/retrieve" + q(lpShadow) + "&k=10"},
		{route: "GET /api/context", path: "/api/context" + q(c["diary"]), kind: lpList, control: "/api/context" + q(lpShadow)},
		{route: "GET /api/complete", path: "/api/complete" + q(c["diary"]), kind: lpList, control: "/api/complete" + q(lpShadow)},
		{route: "GET /api/facts", path: "/api/facts" + q(c["diary"]), kind: lpList},
		{route: "GET /api/blocks", path: "/api/blocks" + q(c["diary"]), kind: lpList},
		{route: "GET /api/tasks", path: "/api/tasks", kind: lpList},
		{route: "GET /api/tags", path: "/api/tags", kind: lpList},
		{route: "GET /api/graph", path: "/api/graph", kind: lpList},
		{route: "GET /api/aliases", path: "/api/aliases", kind: lpList},
		{route: "GET /api/bookmarks", path: "/api/bookmarks", kind: lpList},
		{route: "GET /api/daily", path: "/api/daily", kind: lpList},
		{route: "GET /api/daily/dates", path: "/api/daily/dates", kind: lpList},
		{route: "GET /api/trash", path: "/api/trash", kind: lpList},
		{route: "GET /api/templates", path: "/api/templates", kind: lpList},
		{route: "GET /api/stale", path: "/api/stale", kind: lpList},
		{route: "GET /api/trust", path: "/api/trust", kind: lpList},
		{route: "GET /api/timeline", path: "/api/timeline", kind: lpList},
		{route: "GET /api/briefing", path: "/api/briefing", kind: lpList},
		{route: "GET /api/usage", path: "/api/usage", kind: lpList},
		{route: "GET /api/usage/agents", path: "/api/usage/agents", kind: lpList},
		{route: "GET /api/doctor", path: "/api/doctor", kind: lpList},
		{route: "GET /api/spaces", path: "/api/spaces", kind: lpList},
		{route: "POST /api/query", path: "/api/query", body: map[string]any{"block": "tag: diary"}, kind: lpList},
		{route: "POST /api/template/render", path: "/api/template/render", body: map[string]any{"template": "users/alice/diary.md"}, kind: lpList},
		{route: "POST /api/ask", path: "/api/ask", body: map[string]any{"q": c["diary"]}, kind: lpList},
		{route: "GET /api/knowledge/graph", path: "/api/knowledge/graph", kind: lpList},
		{route: "POST /api/knowledge/query", path: "/api/knowledge/query", body: map[string]any{"question": c["graph-entity"]}, kind: lpList},
		{route: "POST /api/knowledge/extract", path: "/api/knowledge/extract", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "GET /api/knowledge/source", path: "/api/knowledge/source?path=users/alice/diary.md", kind: lpByID},
		{route: "GET /api/search", path: "/api/search" + q("shared-private"), kind: lpList},
		// Member-visible by design (commons, see lpCommonsShared): checked for canaries
		// only. The anonymous rule for it is enforced by the sweeps below.
		{route: "GET /api/notes/{path...}", path: "/api/notes/team/alice-private.md", kind: lpList},
		{route: "GET /api/documents", path: "/api/documents", kind: lpList},
		{route: "GET /api/documents/original", path: "/api/documents/original?path=users/alice/imports/contract.md", kind: lpByID},
		{route: "POST /api/documents/refresh", path: "/api/documents/refresh", body: map[string]any{"path": "users/alice/imports/contract.md"}, kind: lpWrite},
		{route: "POST /api/documents/import", path: "/api/documents/import", body: map[string]any{}, kind: lpWrite},
		{route: "GET /api/export/vault", path: "/api/export/vault", kind: lpList},
		{route: "GET /api/sync/manifest", path: "/api/sync/manifest", kind: lpList},
		{route: "POST /api/sync/pull", path: "/api/sync/pull", body: map[string]any{"paths": []string{"users/alice/diary.md"}}, kind: lpList},
		{route: "POST /api/sync/push", path: "/api/sync/push", body: map[string]any{}, kind: lpWrite},
		{route: "GET /api/sync/status", path: "/api/sync/status", kind: lpList},

		// --- agent memory ---
		{route: "GET /api/memory", path: "/api/memory" + q(c["memory"]), kind: lpList},
		{route: "GET /api/memory", path: "/api/memory?shape=notes&q=vendor", kind: lpList},
		{route: "GET /api/memory", path: "/api/memory?session=sess-alice", kind: lpList},
		{route: "GET /api/memory/context", path: "/api/memory/context?q=vendor", kind: lpList},
		{route: "GET /api/memory/cues", path: "/api/memory/cues", kind: lpList},
		{route: "GET /api/memory/core", path: "/api/memory/core", kind: lpList},
		{route: "GET /api/memory/adherence", path: "/api/memory/adherence", kind: lpList},
		{route: "GET /api/memory/trace", path: "/api/memory/trace?path=users/alice/memory.md", kind: lpByID},
		{route: "GET /api/memory/trace/summary", path: "/api/memory/trace/summary", kind: lpList},
		{route: "GET /api/memory/check", path: "/api/memory/check", kind: lpList},
		{route: "GET /api/memory/rules", path: "/api/memory/rules", kind: lpList},
		{route: "GET /api/memory/impact", path: "/api/memory/impact", kind: lpList},
		{route: "GET /api/memory/replay", path: "/api/memory/replay", kind: lpList},
		{route: "GET /api/memory/export", path: "/api/memory/export", kind: lpList},
		{route: "GET /api/memory/export", path: "/api/memory/export?format=markdown", kind: lpList},
		{route: "GET /api/memory/export", path: "/api/memory/export?format=jsonl", kind: lpList},
		{route: "GET /api/memory/changes", path: "/api/memory/changes?since=1970-01-01T00:00:00Z", kind: lpList},
		{route: "GET /api/memory/stream", path: "/api/memory/stream", kind: lpList},
		{route: "GET /api/memory/profile", path: "/api/memory/profile?subject=alice", kind: lpList},
		{route: "GET /api/memory/profile", path: "/api/memory/profile?subject=alice&synthesize=true", kind: lpList},
		{route: "GET /api/memory/facets", path: "/api/memory/facets", kind: lpList},
		{route: "GET /api/memory/graph", path: "/api/memory/graph", kind: lpList},
		{route: "GET /api/memory/challenges", path: "/api/memory/challenges", kind: lpList},
		{route: "POST /api/memory/search", path: "/api/memory/search", body: map[string]any{"vector": []float64{0.1, 0.2}}, kind: lpList},
		{route: "POST /api/memory/batch", path: "/api/memory/batch", body: map[string]any{"items": []map[string]any{}}, kind: lpWrite},
		{route: "POST /api/memory/feedback", path: "/api/memory/feedback", body: map[string]any{"id": "x"}, kind: lpWrite},
		{route: "POST /api/memory/prune", path: "/api/memory/prune", body: map[string]any{"apply": false}, kind: lpList},
		{route: "POST /api/memory/forget", path: "/api/memory/forget", body: map[string]any{"id": "x", "cascade": true, "dry_run": true}, kind: lpWrite},
		{route: "GET /api/memory/receipts", path: "/api/memory/receipts", kind: lpList},
		{route: "PATCH /api/memory/entry", path: "/api/memory/entry", method: "PATCH", body: map[string]any{"text": "x"}, kind: lpWrite},
		{route: "DELETE /api/memory/entry", path: "/api/memory/entry?id=x", method: "DELETE", kind: lpWrite},
		{route: "POST /api/memory/challenge", path: "/api/memory/challenge", body: map[string]any{"note": "memory/infra.md", "id": "x", "resolution": "concede"}, kind: lpWrite},
		{route: "POST /api/memory/consolidate", path: "/api/memory/consolidate", body: map[string]any{"path": "users/alice/memory.md"}, kind: lpWrite},
		{route: "POST /api/memory/outcome", path: "/api/memory/outcome", body: map[string]any{"text": "x"}, kind: lpWrite},
		{route: "POST /api/memory/cues", path: "/api/memory/cues", body: map[string]any{"target_path": "users/alice/memory.md"}, kind: lpWrite},
		{route: "POST /api/memory/check", path: "/api/memory/check", body: map[string]any{"target_path": "users/alice/memory.md"}, kind: lpWrite},
		{route: "DELETE /api/memory/check", path: "/api/memory/check", method: "DELETE", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/memory/check/accept", path: "/api/memory/check/accept", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/memory", path: "/api/memory", body: map[string]any{"text": "x", "target_path": "users/alice/memory.md"}, kind: lpWrite},
		{route: "POST /api/memory/import", path: "/api/memory/import", body: map[string]any{"facts": []any{}}, kind: lpWrite},

		// --- banks: a bank the caller cannot read answers as absent ---
		{route: "GET /api/banks", path: "/api/banks", kind: lpList},
		{route: "GET /api/banks/{bank}", path: "/api/banks/alicebank", kind: lpByID},
		{route: "GET /api/banks/{bank}/memories", path: "/api/banks/alicebank/memories?q=Priya", kind: lpByID, control: "/api/banks/alicebank/memories?q=Shadowxyz"},
		{route: "GET /api/banks/{bank}/index", path: "/api/banks/alicebank/index", kind: lpByID},
		{route: "GET /api/banks/{bank}/timeline", path: "/api/banks/alicebank/timeline", kind: lpByID},
		{route: "GET /api/banks/{bank}/file-memory", path: "/api/banks/alicebank/file-memory?path=bank.md", kind: lpByID},
		{route: "GET /api/banks/{bank}/duplicates", path: "/api/banks/alicebank/duplicates", kind: lpByID},
		{route: "GET /api/banks/{bank}/lookup", path: "/api/banks/alicebank/lookup?q=Priya", kind: lpByID},
		{route: "GET /api/banks/{bank}/sessions", path: "/api/banks/alicebank/sessions", kind: lpByID},
		{route: "GET /api/banks/{bank}/context", path: "/api/banks/alicebank/context", kind: lpByID},
		{route: "GET /api/banks/{bank}/entities", path: "/api/banks/alicebank/entities", kind: lpByID},
		{route: "GET /api/banks/{bank}/entities/{id}", path: "/api/banks/alicebank/entities/priya", kind: lpByID},
		{route: "GET /api/banks/{bank}/documents", path: "/api/banks/alicebank/documents", kind: lpByID},
		{route: "GET /api/banks/{bank}/stats", path: "/api/banks/alicebank/stats", kind: lpByID},
		{route: "GET /api/banks/{bank}/observations", path: "/api/banks/alicebank/observations", kind: lpByID},
		{route: "GET /api/banks/{bank}/mental-models", path: "/api/banks/alicebank/mental-models", kind: lpByID},
		{route: "GET /api/banks/{bank}/mental-models-tree", path: "/api/banks/alicebank/mental-models-tree", kind: lpByID},
		{route: "GET /api/banks/{bank}/mental-models-export", path: "/api/banks/alicebank/mental-models-export", kind: lpByID},
		{route: "GET /api/banks/{bank}/mental-models/{id}", path: "/api/banks/alicebank/mental-models/priya", kind: lpByID},
		{route: "GET /api/banks/{bank}/mental-models/{id}/history", path: "/api/banks/alicebank/mental-models/priya/history", kind: lpByID},
		{route: "GET /api/banks/{bank}/directives", path: "/api/banks/alicebank/directives", kind: lpByID},
		{route: "GET /api/banks/{bank}/operations", path: "/api/banks/alicebank/operations", kind: lpByID},
		{route: "GET /api/banks/{bank}/webhooks", path: "/api/banks/alicebank/webhooks", kind: lpByID},
		{route: "GET /api/banks/{bank}/export", path: "/api/banks/alicebank/export", kind: lpByID},
		{route: "GET /api/banks/{bank}/sessions", path: "/api/banks/alicebank/sessions", kind: lpByID},
		{route: "POST /api/banks/{bank}/memories/recall", path: "/api/banks/alicebank/memories/recall", body: map[string]any{"query": "Priya Initech"}, kind: lpByID, control: "/api/banks/alicebank/memories/recall"},
		{route: "POST /api/banks/{bank}/reflect", path: "/api/banks/alicebank/reflect", body: map[string]any{"query": "Priya"}, kind: lpByID},
		{route: "POST /api/banks/{bank}/duplicates/merge", path: "/api/banks/alicebank/duplicates/merge", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/memories", path: "/api/banks/alicebank/memories", body: map[string]any{"items": []map[string]any{{"content": "intrusion"}}}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/consolidate", path: "/api/banks/alicebank/consolidate", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/mental-models", path: "/api/banks/alicebank/mental-models", body: map[string]any{"id": "intrusion", "content": "x"}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/directives", path: "/api/banks/alicebank/directives", body: map[string]any{"name": "x", "text": "x"}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/import", path: "/api/banks/alicebank/import", body: map[string]any{}, kind: lpWrite},
		{route: "PATCH /api/banks/{bank}", path: "/api/banks/alicebank", method: "PATCH", body: map[string]any{"mission": "x"}, kind: lpWrite},
		{route: "DELETE /api/banks/{bank}", path: "/api/banks/alicebank", method: "DELETE", kind: lpWrite},

		// --- publishing: only the publish:true, not-private note may leave ---
		{route: "GET /published", path: "/published", kind: lpList},
		{route: "GET /published/{path...}", path: "/published/users/alice/pubprivate.md", kind: lpByID},
		{route: "GET /published/{path...}", path: "/published/users/alice/diary.md", kind: lpByID},
		{route: "GET /api/published", path: "/api/published", kind: lpList},

		// --- the rest of the surface, each read or refused for a non-owner ---
		{route: "GET /api/health", path: "/api/health", kind: lpList},
		{route: "GET /api/me", path: "/api/me", kind: lpList},
		{route: "GET /api/identity", path: "/api/identity", kind: lpList},
		{route: "GET /metrics", path: "/metrics", kind: lpList},
		{route: "GET /api/bank-templates", path: "/api/bank-templates", kind: lpList},
		{route: "GET /api/bank-templates/{id}", path: "/api/bank-templates/x", kind: lpList},
		{route: "GET /api/keys", path: "/api/keys", kind: lpList},
		{route: "GET /api/plugins", path: "/api/plugins", kind: lpList},
		{route: "GET /api/vault/status", path: "/api/vault/status", kind: lpList},
		{route: "GET /api/web/search", path: "/api/web/search?q=x", kind: lpList},
		{route: "GET /api/secrets/requests/{id}", path: "/api/secrets/requests/x", kind: lpByID},
		{route: "POST /api/secrets/requests", path: "/api/secrets/requests", body: map[string]any{"name": "x"}, kind: lpWrite},
		{route: "POST /api/secrets/broker", path: "/api/secrets/broker", body: map[string]any{"token": "x"}, kind: lpByID},
		{route: "GET /api/audit", path: "/api/audit", kind: lpByID},
		{route: "GET /api/admin/reads", path: "/api/admin/reads", kind: lpByID},
		{route: "GET /api/admin/reads/anomalies", path: "/api/admin/reads/anomalies", kind: lpByID},
		{route: "GET /api/source-audit", path: "/api/source-audit", kind: lpByID},
		// --- remaining writes and by-id reads: each route answers with no owned byte ---
		{route: "DELETE /api/banks/{bank}/directives/{id}", path: "/api/banks/alicebank/directives/rule", method: "DELETE", kind: lpWrite},
		{route: "PATCH /api/banks/{bank}/directives/{id}", path: "/api/banks/alicebank/directives/rule", method: "PATCH", body: map[string]any{"text": "intrusion"}, kind: lpWrite},
		{route: "DELETE /api/banks/{bank}/documents/{id}", path: "/api/banks/alicebank/documents/" + lpOr(w.ids["document"], "probe"), method: "DELETE", kind: lpWrite},
		{route: "GET /api/banks/{bank}/documents/{id}", path: "/api/banks/alicebank/documents/" + lpOr(w.ids["document"], "probe"), kind: lpByID},
		{route: "GET /api/banks/{bank}/chunks/{id}", path: "/api/banks/alicebank/chunks/" + lpOr(w.ids["document"], "probe"), kind: lpByID},
		{route: "GET /api/banks/{bank}/memories/{id}", path: "/api/banks/alicebank/memories/" + w.ids["memory"], kind: lpByID},
		{route: "DELETE /api/banks/{bank}/memories/{id}", path: "/api/banks/alicebank/memories/" + w.ids["memory"], method: "DELETE", kind: lpWrite},
		{route: "DELETE /api/banks/{bank}/mental-models/{id}", path: "/api/banks/alicebank/mental-models/priya", method: "DELETE", kind: lpWrite},
		{route: "PATCH /api/banks/{bank}/mental-models/{id}", path: "/api/banks/alicebank/mental-models/priya", method: "PATCH", body: map[string]any{"name": "intrusion"}, kind: lpWrite},
		{route: "GET /api/banks/{bank}/mental-models/{id}/history/{version}", path: "/api/banks/alicebank/mental-models/priya/history/1", kind: lpByID},
		{route: "POST /api/banks/{bank}/mental-models/{id}/refresh", path: "/api/banks/alicebank/mental-models/priya/refresh", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/mental-models/{id}/proposal/accept", path: "/api/banks/alicebank/mental-models/priya/proposal/accept", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/mental-models/{id}/proposal/reject", path: "/api/banks/alicebank/mental-models/priya/proposal/reject", body: map[string]any{}, kind: lpWrite},
		{route: "DELETE /api/banks/{bank}/observations", path: "/api/banks/alicebank/observations", method: "DELETE", kind: lpWrite},
		{route: "GET /api/banks/{bank}/observations/{id}", path: "/api/banks/alicebank/observations/" + lpOr(w.ids["observation"], "probe"), kind: lpByID},
		{route: "PATCH /api/banks/{bank}/observations/{id}", path: "/api/banks/alicebank/observations/" + lpOr(w.ids["observation"], "probe"), method: "PATCH", body: map[string]any{"text": "intrusion"}, kind: lpWrite},
		{route: "DELETE /api/banks/{bank}/observations/{id}", path: "/api/banks/alicebank/observations/" + lpOr(w.ids["observation"], "probe"), method: "DELETE", kind: lpWrite},
		{route: "GET /api/banks/{bank}/operations/{id}", path: "/api/banks/alicebank/operations/" + lpOr(w.ids["operation"], "probe"), kind: lpByID},
		{route: "DELETE /api/banks/{bank}/operations/{id}", path: "/api/banks/alicebank/operations/" + lpOr(w.ids["operation"], "probe"), method: "DELETE", kind: lpWrite},
		{route: "GET /api/banks/{bank}/webhooks/{id}/deliveries", path: "/api/banks/alicebank/webhooks/" + lpOr(w.ids["webhook"], "probe") + "/deliveries", kind: lpByID},
		{route: "PATCH /api/banks/{bank}/webhooks/{id}", path: "/api/banks/alicebank/webhooks/" + lpOr(w.ids["webhook"], "probe"), method: "PATCH", body: map[string]any{"url": "https://example.invalid/x"}, kind: lpWrite},
		{route: "DELETE /api/banks/{bank}/webhooks/{id}", path: "/api/banks/alicebank/webhooks/" + lpOr(w.ids["webhook"], "probe"), method: "DELETE", kind: lpWrite},
		{route: "POST /api/banks/{bank}/webhooks", path: "/api/banks/alicebank/webhooks", body: map[string]any{"url": "https://example.invalid/x"}, kind: lpWrite},
		{route: "POST /api/banks/{bank}/sessions/{session}/digest", path: "/api/banks/alicebank/sessions/sess-alice/digest", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/banks", path: "/api/banks", body: map[string]any{"bank_id": "zzprobebank"}, kind: lpWrite},
		{route: "POST /api/bookmarks", path: "/api/bookmarks", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "DELETE /api/bookmarks", path: "/api/bookmarks?path=users/alice/diary.md", method: "DELETE", kind: lpWrite},
		{route: "POST /api/keys", path: "/api/keys", body: map[string]any{"label": "probe"}, kind: lpWrite},
		{route: "DELETE /api/keys/{id}", path: "/api/keys/probe", method: "DELETE", kind: lpWrite},
		{route: "DELETE /api/trash/{tid}", path: "/api/trash/" + lpOr(w.ids["trash"], "probe"), method: "DELETE", kind: lpWrite},
		{route: "POST /api/trash/{tid}/restore", path: "/api/trash/" + lpOr(w.ids["trash"], "probe") + "/restore", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/actions", path: "/api/actions", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/attach", path: "/api/attach", kind: lpWrite},
		{route: "POST /api/audio", path: "/api/audio", kind: lpWrite},
		{route: "POST /api/capture", path: "/api/capture", body: map[string]any{"text": "probe"}, kind: lpWrite},
		{route: "POST /api/embed", path: "/api/embed", body: map[string]any{"text": "probe"}, kind: lpList},
		{route: "POST /api/facts", path: "/api/facts", body: map[string]any{"note": "users/alice/diary.md", "key": "k", "value": "intrusion"}, kind: lpWrite},
		{route: "POST /api/import/vault", path: "/api/import/vault", body: map[string]any{}, kind: lpWrite},
		{route: "POST /api/stale/verify", path: "/api/stale/verify", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "POST /api/tags/rename", path: "/api/tags/rename", body: map[string]any{"from": "diary", "to": "intrusion"}, kind: lpWrite},
		{route: "POST /api/templates", path: "/api/templates", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "POST /api/templates/apply", path: "/api/templates/apply", body: map[string]any{"template": "users/alice/diary.md", "title": "probe"}, kind: lpWrite},
		{route: "POST /api/trust/vouch", path: "/api/trust/vouch", body: map[string]any{"path": "users/alice/diary.md"}, kind: lpWrite},
		{route: "POST /api/web/fetch", path: "/api/web/fetch", body: map[string]any{"url": "http://127.0.0.1:1/"}, kind: lpWrite},
	}
}

// lpOr returns id, or fallback when seeding captured none.
func lpOr(id, fallback string) string {
	if id == "" {
		return fallback
	}
	return id
}

// lpExempt lists routes whose responses never contain owned content and that
// are therefore not worth a per-item probe. Each needs a reason. Admin-class
// routes are not here: TestLeakprobeAdminRoutesRefuseMembers drives every one.
var lpExempt = map[string]string{
	"POST /api/users":              "creates the first account; admin-gated after that, and it returns no content",
	"POST /api/auth/login":         "sign-in only; the failure path is covered by login_test and lockout tests",
	"POST /api/auth/logout":        "ends a session; returns nothing",
	"POST /api/auth/password":      "changes the caller's own password; returns nothing",
	"GET /plugins/{name}/{rel...}": "static plugin assets, no owned content",
}

// The SSE stream is held open by the server, so it cannot be read to EOF. A
// request is cut off after this long and whatever arrived is what counts.
const lpStreamWindow = 400 * time.Millisecond

// lpFetch performs a request as an identity over the real HTTP listener, so a
// streaming handler can be read for a bounded time.
func (w *lpWorld) fetch(who lpWho, method, path string, body any) (int, string) {
	w.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lpStreamWindow)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, w.srv.URL+path, rdr)
	if err != nil {
		w.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if who.key != "" {
		req.Header.Set("Authorization", "Bearer "+who.key)
	}
	if who.agent != "" {
		req.Header.Set("X-Grimoire-Agent", who.agent)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// A timed-out stream that sent nothing is the best outcome a stream can
		// have for a non-owner.
		return 0, ""
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// lpAllowed is the set of canary labels this identity may see on this route.
// Anything else in a body is a leak. Nothing is allowed by default.
func lpAllowed(who lpWho, route string) map[string]bool {
	allowed := map[string]bool{}
	if lpPublicRoute(route) {
		allowed["published"] = true // the one note the operator published
	}
	if who.key != "" {
		for label := range lpCommonsShared {
			allowed[label] = true // members read the commons by design
		}
	}
	return allowed
}

// lpCanaryHits names every canary present in a body that is not allowed.
//
// A response may repeat what the request said: a question echoed back in
// "From your notes on ..." is not a leak, and a probe that asks for a canary
// by name must not be failed for its own question. So a canary counts as
// leaked only where the body carries MORE copies than the request did.
func lpCanaryHits(body string, allowed map[string]bool) []string {
	return lpLeaked(body, "", allowed)
}

// lpLeaked is lpCanaryHits with the request text that produced the body.
func lpLeaked(body, request string, allowed map[string]bool) []string {
	var hits []string
	for label, c := range lpCanaries {
		if allowed[label] {
			continue
		}
		if strings.Count(body, c) > strings.Count(request, c) {
			hits = append(hits, label)
		}
	}
	sort.Strings(hits)
	return hits
}

// lpRequestText is everything a probe sent, for echo detection.
func lpRequestText(path string, body any) string {
	b, _ := json.Marshal(body)
	return path + " " + string(b)
}

var lpCountKey = regexp.MustCompile(`(?i)(^|_)(total|count|counts|n|degree|facets?|hits|size|entities|tags|edges|nodes|documents|facts|memories|observations|models|len|num)$`)

// lpCounts collects every count-like number in a JSON body, keyed by its path,
// and the length of every top-level array. Two responses that a non-owner can
// tell apart by these numbers have leaked the hidden matches' existence.
func lpCounts(body string) map[string]float64 {
	out := map[string]float64{}
	var v any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return out
	}
	var walk func(path string, x any)
	walk = func(path string, x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, child := range t {
				p := path + "." + k
				if f, ok := child.(float64); ok && lpCountKey.MatchString(k) {
					out[p] = f
				}
				if arr, ok := child.([]any); ok {
					out[p+"#len"] = float64(len(arr))
				}
				walk(p, child)
			}
		case []any:
			for i, child := range t {
				walk(fmt.Sprintf("%s[%d]", path, i), child)
			}
		}
	}
	walk("", v)
	if arr, ok := v.([]any); ok {
		out["#len"] = float64(len(arr))
	}
	return out
}

// lpDiffCounts reports count fields that differ between a hidden-matching
// query and the control query.
func lpDiffCounts(a, b map[string]float64) []string {
	var diffs []string
	seen := map[string]bool{}
	for k, av := range a {
		seen[k] = true
		if bv, ok := b[k]; !ok || bv != av {
			diffs = append(diffs, fmt.Sprintf("%s: %v vs control %v", k, av, b[k]))
		}
	}
	for k, bv := range b {
		if !seen[k] {
			diffs = append(diffs, fmt.Sprintf("%s: absent vs control %v", k, bv))
		}
	}
	sort.Strings(diffs)
	return diffs
}

// lpPublicRoute reports whether a route is part of the published site, the one
// surface that is meant to answer anonymous callers with note content.
func lpPublicRoute(route string) bool {
	return route == "GET /published" || strings.HasPrefix(route, "GET /published/") || route == "GET /api/published"
}

// lpRunProbe is the single place a probe's assertions live.
func lpRunProbe(t *testing.T, w *lpWorld, p lpProbe) {
	t.Helper()
	method := p.method
	if method == "" {
		method = strings.SplitN(p.route, " ", 2)[0]
	}
	public := lpPublicRoute(p.route)
	for _, who := range w.who() {
		t.Run(who.name+" "+p.route+" "+p.path, func(t *testing.T) {
			code, body := w.fetch(who, method, p.path, p.body)
			if hits := lpLeaked(body, lpRequestText(p.path, p.body), lpAllowed(who, p.route)); len(hits) > 0 {
				t.Fatalf("LEAK: %s as %s got owned canaries %v (status %d): %.400s", p.path, who.name, hits, code, body)
			}
			if strings.Contains(body, lpCanaries["pubprivate"]) {
				t.Fatalf("LEAK: a note that is both published and private was served as %s on %s", who.name, p.path)
			}
			if p.kind == lpByID && code >= 200 && code < 300 && !public {
				t.Fatalf("%s as %s answered %d; a non-owner must get 404/403/401, not success", p.path, who.name, code)
			}
			if p.control != "" {
				_, ctl := w.fetch(who, method, p.control, p.body)
				if diffs := lpDiffCounts(lpCounts(body), lpCounts(ctl)); len(diffs) > 0 {
					t.Fatalf("SIDE CHANNEL: %s as %s — counts move with hidden matches: %v", p.path, who.name, diffs)
				}
			}
		})
	}
}

// TestLeakprobeExplicitRoutes drives every explicit probe as every identity.
func TestLeakprobeExplicitRoutes(t *testing.T) {
	w := seedLeakWorld(t)
	for _, p := range lpProbes(w) {
		lpRunProbe(t, w, p)
	}
	// The probes must not have changed what alice owns: a write that slipped
	// through would make the suite pass against a smaller or altered fixture.
	for _, check := range []struct{ label, path string }{
		{"diary", "/api/notes/users/alice/diary.md"},
		{"private", "/api/notes/users/alice/private.md"},
	} {
		if code, body := w.as(w.alice, "GET", check.path, nil); code != 200 || !strings.Contains(body, lpCanaries[check.label]) {
			t.Fatalf("alice lost %s after the probe sweep (%d): a probe mutated owned data", check.label, code)
		}
	}
	// Every write probe used a marker word. None may appear in alice's data.
	_, diary := w.as(w.alice, "GET", "/api/notes/users/alice/diary.md", nil)
	if strings.Contains(diary, "overwritten") {
		t.Fatal("a non-owner's PUT rewrote alice's diary")
	}
	if _, bank := w.as(w.alice, "GET", "/api/banks/alicebank/directives", nil); strings.Contains(bank, "intrusion") {
		t.Fatal("a non-owner's write changed alice's bank")
	}
}

// TestLeakprobeRouteCoverage is the failure that stops a new route shipping
// unprobed. The route table comes from the source, as route_audit_test does it.
func TestLeakprobeRouteCoverage(t *testing.T) {
	covered := map[string]bool{}
	w := seedLeakWorld(t)
	for _, p := range lpProbes(w) {
		covered[p.route] = true
	}
	for route := range lpExempt {
		covered[route] = true
	}
	for _, route := range registeredRoutes(t) {
		if covered[route] {
			continue
		}
		if class, ok := routeAccess[route]; ok && class == admin {
			continue // TestLeakprobeAdminRoutesRefuseMembers drives every admin route
		}
		t.Errorf("route %q has no leakage probe and no exemption.\n"+
			"Add an lpProbes entry that asserts no owned canary reaches a non-owner, or an "+
			"lpExempt entry with the reason it cannot.", route)
	}
	registered := map[string]bool{}
	for _, route := range registeredRoutes(t) {
		registered[route] = true
	}
	for route := range covered {
		if !registered[route] {
			// a probe for a route that is no longer registered is a typo or a stale entry
			t.Errorf("probe names %q, which is not a registered route", route)
		}
	}
}

// TestLeakprobeAdminRoutesRefuseMembers: every admin-class route refuses a
// member, an anonymous caller, and an agent identity without a grant, and no
// canary comes back from any of them.
func TestLeakprobeAdminRoutesRefuseMembers(t *testing.T) {
	w := seedLeakWorld(t)
	for route, class := range routeAccess {
		if class != admin {
			continue
		}
		method, pattern, _ := strings.Cut(route, " ")
		path := lpFill(pattern)
		for _, who := range w.who() {
			t.Run(who.name+" "+route, func(t *testing.T) {
				code, body := w.fetch(who, method, path, lpBodyFor(route))
				if hits := lpLeaked(body, lpRequestText(path, lpBodyFor(route)), lpAllowed(who, route)); len(hits) > 0 {
					t.Fatalf("LEAK: admin route %s answered %s with canaries %v", route, who.name, hits)
				}
				if code >= 200 && code < 300 {
					t.Fatalf("admin route %s answered %s with %d", route, who.name, code)
				}
			})
		}
	}
}

// lpFill replaces wildcards with stand-ins that name alice's objects where a
// route can take one. Values only need to be well-formed for the refusal to be
// the thing under test.
func lpFill(pattern string) string {
	r := strings.NewReplacer(
		"{path...}", "users/alice/diary.md",
		"{rel...}", "probe.js",
		"{name}", "alicebank",
		"{bank}", "alicebank",
		"{id}", "priya",
		"{tid}", "probe",
		"{source}", "probe",
		"{external}", "probe",
		"{user}", "x",
		"{version}", "1",
		"{token}", "probe",
		"{session}", "sess-alice",
	)
	return r.Replace(pattern)
}

// lpBodyFor is a body good enough to reach the branch under test.
func lpBodyFor(route string) any {
	switch route {
	case "POST /api/vault/change-passphrase":
		return map[string]any{"old": "probe-old", "new": "probe-new"}
	case "POST /api/spaces":
		return map[string]any{"Name": "probe", "Prefix": "probe"}
	case "POST /api/spaces/{id}/members":
		return map[string]any{"User": "x", "Role": "writer"}
	case "POST /api/users", "PUT /api/users/{id}":
		return map[string]any{"name": "probe", "password": "correct horse battery", "role": "admin"}
	}
	return map[string]any{}
}

// TestLeakprobeGenericSweep drives every registered route with concrete
// stand-ins, so a route with no explicit entry still gets its canary check.
func TestLeakprobeGenericSweep(t *testing.T) {
	w := seedLeakWorld(t)
	for _, route := range registeredRoutes(t) {
		method, pattern, ok := strings.Cut(route, " ")
		if !ok {
			method, pattern = "GET", route
		}
		path := lpFill(pattern)
		for _, who := range w.who() {
			t.Run(who.name+" "+route, func(t *testing.T) {
				code, body := w.fetch(who, method, path, lpBodyFor(route))
				if hits := lpLeaked(body, lpRequestText(path, lpBodyFor(route)), lpAllowed(who, route)); len(hits) > 0 {
					t.Fatalf("LEAK: %s %s as %s returned canaries %v (status %d)", method, path, who.name, hits, code)
				}
			})
		}
	}
}

// TestLeakprobeMCPDispatcher calls the in-process MCP server as bob, as an
// anonymous caller, and as an agent without a grant, for every tool it
// advertises. The MCP layer is an HTTP client of the API, so a refusal there
// must come from the same checks, and nothing may come back in a result.
func TestLeakprobeMCPDispatcher(t *testing.T) {
	w := seedLeakWorld(t)
	args := map[string]any{
		"query": "Priya diary vendor infra", "q": "diary", "text": "probe",
		"path": "users/alice/diary.md", "note": "users/alice/diary.md", "id": "priya",
		"bank": "alicebank", "name": "priya", "topic": "vendor", "subject": "alice",
		"session": "sess-alice", "agent": "alice-agent", "k": 10,
	}
	for _, who := range w.who() {
		if who.name == "bob+agent" || who.name == "anon+agent" {
			continue // the MCP client sends its own agent name; covered by bob/anon
		}
		m := mcp.New(w.srv.URL, "rogue-agent")
		m.AuthToken = who.key
		names := mcpToolNames(t, m)
		if len(names) < 30 {
			t.Fatalf("MCP advertised only %d tools", len(names))
		}
		for _, name := range names {
			t.Run(who.name+" "+name, func(t *testing.T) {
				res := mcpCall(t, m, name, args)
				if hits := lpLeaked(res, lpRequestText(name, args), lpAllowed(who, "MCP")); len(hits) > 0 {
					t.Fatalf("LEAK: MCP tool %s as %s returned canaries %v: %.300s", name, who.name, hits, res)
				}
			})
		}
	}
}

func mcpToolNames(t *testing.T, m *mcp.Server) []string {
	t.Helper()
	var in, out bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	if err := m.Serve(&in, &out); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &resp); err != nil {
		t.Fatalf("tools/list: %v: %s", err, out.String())
	}
	var names []string
	for _, tl := range resp.Result.Tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

func mcpCall(t *testing.T, m *mcp.Server, name string, args map[string]any) string {
	t.Helper()
	var in, out bytes.Buffer
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	in.Write(b)
	in.WriteByte('\n')
	if err := m.Serve(&in, &out); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// TestLeakprobeFixtureIsLive proves the channels the probes rely on actually
// deliver content to whoever is entitled to it. A probe that reaches a surface
// that answers every caller with nothing would pass for the wrong reason.
func TestLeakprobeFixtureIsLive(t *testing.T) {
	w := seedLeakWorld(t)
	c := lpCanaries

	// The published site serves the one note marked publish and not private.
	if code, body := w.do(lpWho{}, "GET", "/published/users/alice/published.md", nil); code != 200 || !strings.Contains(body, lpPublicOK) {
		t.Fatalf("the published note is not served anonymously (%d): the public allowance is untested", code)
	}
	if code, _ := w.do(lpWho{}, "GET", "/published/users/alice/pubprivate.md", nil); code == 200 {
		t.Fatal("a note marked publish AND private is served anonymously")
	}

	// The SSE stream replays from Last-Event-ID, so an owner who resumes from
	// the epoch must receive her own memory. Without that the stream probe
	// would be looking at an empty channel.
	req, _ := http.NewRequest("GET", w.srv.URL+"/api/memory/stream", nil)
	req.Header.Set("Authorization", "Bearer "+w.alice)
	req.Header.Set("Last-Event-ID", "2000-01-01 00:00|a|b")
	ctx, cancel := context.WithTimeout(context.Background(), lpStreamWindow)
	defer cancel()
	req = req.WithContext(ctx)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if !strings.Contains(string(raw), c["memory"]) && !strings.Contains(string(raw), c["agent-mem"]) {
			t.Fatalf("alice's replayed memory stream carries none of her memory: the stream probe is untested: %.200s", raw)
		}
	} else {
		t.Fatal(err)
	}

	// The export carries the owner's memory in every format it offers.
	for _, f := range []string{"", "?format=jsonl", "?format=markdown"} {
		if _, body := w.as(w.alice, "GET", "/api/memory/export"+f, nil); !strings.Contains(body, c["memory"]) {
			t.Fatalf("alice's export%s does not carry her memory: the export probe is untested", f)
		}
	}

	// MCP forwards as the caller's own key: bob sees bob's note, anon does not.
	m := mcp.New(w.srv.URL, "rogue-agent")
	m.AuthToken = w.bob
	if res := mcpCall(t, m, "search_notes", map[string]any{"query": "kestrel", "q": "kestrel"}); !strings.Contains(res, "bob/hello") {
		t.Fatalf("MCP as bob does not reach bob's own note: %.300s", res)
	}
	anon := mcp.New(w.srv.URL, "rogue-agent")
	if res := mcpCall(t, anon, "search_notes", map[string]any{"query": "kestrel", "q": "kestrel"}); strings.Contains(res, "bob/hello") {
		t.Fatalf("MCP as anonymous reads a member's note: %.300s", res)
	}
}

// TestLeakprobeNoExistenceOracle: for a non-owner, an object that exists must
// answer exactly as one that does not. A different status or body shape is a
// way to enumerate owned names without ever reading them.
func TestLeakprobeNoExistenceOracle(t *testing.T) {
	w := seedLeakWorld(t)
	pairs := []struct {
		name, real, fake string
		method           string
		body             any
	}{
		{"note by path", "/api/notes/users/alice/diary.md", "/api/notes/users/alice/nothere.md", "GET", nil},
		{"private note by path", "/api/notes/users/alice/private.md", "/api/notes/users/alice/nothere.md", "GET", nil},
		{"note history", "/api/notes/users/alice/versioned.md/history", "/api/notes/users/alice/nothere.md/history", "GET", nil},
		{"bank", "/api/banks/alicebank", "/api/banks/nothere", "GET", nil},
		{"bank memory", "/api/banks/alicebank/memories/" + w.ids["memory"], "/api/banks/alicebank/memories/nothere", "GET", nil},
		{"mental model", "/api/banks/alicebank/mental-models/priya", "/api/banks/alicebank/mental-models/nothere", "GET", nil},
		{"directive", "/api/banks/alicebank/directives/rule", "/api/banks/alicebank/directives/nothere", "DELETE", nil},
		// Creating a bank is not compared: a create under a space the caller cannot
		// write answers the same whether or not the name is taken, so it reveals
		// the space, not the bank.
		{"trash item", "/api/trash/" + lpOr(w.ids["trash"], "probe"), "/api/trash/nothere", "DELETE", nil},
	}
	for _, p := range pairs {
		t.Run(p.name, func(t *testing.T) {
			for _, who := range []lpWho{{name: "bob", key: w.bob}, {name: "anon"}} {
				realCode, _ := w.do(who, p.method, p.real, p.body)
				fakeCode, _ := w.do(who, p.method, p.fake, p.body)
				if realCode != fakeCode {
					t.Errorf("%s: an existing object answers %d and a missing one %d — that difference is an existence oracle",
						who.name, realCode, fakeCode)
				}
			}
		})
	}
}

// TestLeakprobeCountsDoNotDependOnHiddenOwnedItems is the differential check
// for count fields. The full world and a control world hold the same commons
// content, and differ only in alice's owner-only items. A non-owner must get
// the same counts from both, on every list and by-id surface: totals, facets,
// graph degree, tag and entity counts, array lengths. A count that moves is a
// count of something the caller cannot read.
func TestLeakprobeCountsDoNotDependOnHiddenOwnedItems(t *testing.T) {
	full := seedLeakWorldWith(t, true)
	control := seedLeakWorldWith(t, false)
	for _, p := range lpProbes(full) {
		if p.kind == lpWrite || p.route == "GET /api/memory/stream" || strings.Contains(p.path, "export") {
			continue // writes change state; the stream and exports are not JSON counts
		}
		if lpPublicRoute(p.route) {
			continue // the published site lists the operator's published notes to everyone, by design
		}
		method := p.method
		if method == "" {
			method = strings.SplitN(p.route, " ", 2)[0]
		}
		for _, who := range []lpWho{{name: "bob", key: full.bob}, {name: "anon"}} {
			t.Run(who.name+" "+p.path, func(t *testing.T) {
				_, fb := full.fetch(who, method, p.path, p.body)
				// the same probe against the control world, with the same identity
				cwho := lpWho{name: who.name, key: control.bob, agent: who.agent}
				if who.key == "" {
					cwho = lpWho{name: who.name}
				}
				_, cb := control.fetch(cwho, method, p.path, p.body)
				if diffs := lpDiffCounts(lpCounts(fb), lpCounts(cb)); len(diffs) > 0 {
					t.Fatalf("COUNT LEAK: %s as %s — counts differ from a world with no hidden owned items: %v", p.path, who.name, diffs)
				}
			})
		}
	}
}

// Recall above the scan bound takes its candidates from the FTS table and the
// vector and entity arms, not from the newest rows. Those arms read the whole
// store, so each must apply the caller's visibility before it ranks; otherwise a
// hidden fact could take a candidate slot from a visible one. The bound is
// forced to 3 here so that the small seeded world takes the candidate path for
// alice, and both candidate and window paths are probed for bob and anonymous.
func TestLeakprobeMemoryRecallOverScanBound(t *testing.T) {
	saved := index.DefaultScanLimit
	index.DefaultScanLimit = 3
	t.Cleanup(func() { index.DefaultScanLimit = saved })

	w := seedLeakWorld(t)
	paths := []string{
		"/api/memory?q=vendor",
		"/api/memory?q=diary+private+vendor",
		"/api/memory?q=agent+memory+vendor",
		"/api/memory/context?q=vendor",
	}
	for _, path := range paths {
		for _, who := range w.who() {
			code, body := w.fetch(who, "GET", path, nil)
			if hits := lpLeaked(body, lpRequestText(path, nil), lpAllowed(who, "GET /api/memory")); len(hits) > 0 {
				t.Fatalf("LEAK: GET %s over the scan bound answered %s (status %d) with canaries %v",
					path, who.name, code, hits)
			}
		}
	}
	// The probe is meaningless if alice's own recall stops finding her memory
	// on the candidate path.
	if code, body := w.as(w.alice, "GET", "/api/memory?q=vendor", nil); code != 200 || !strings.Contains(body, lpCanaries["memory"]) {
		t.Fatalf("alice cannot recall her own memory over the scan bound (%d): the probe proves nothing", code)
	}
}
