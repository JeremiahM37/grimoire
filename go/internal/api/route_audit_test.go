package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// Every route must be classified, and adding one must FAIL until it is.
//
// This is the check that would have prevented the whole class of holes found
// over the last day. Sync, /read, the vault export, push, attach and plugin
// install were all written before the access model existed, kept working
// exactly as they always had, and each was discovered one at a time by somebody
// going and looking. Nothing in the build knew a route existed, let alone
// whether anyone had thought about who may call it.
//
// So the route table is read from the source and cross-checked against a
// declaration below. A new mux.HandleFunc with no entry here fails this test,
// which turns "did you remember to check authorization?" from a habit into a
// build error. It cannot verify that a handler's check is CORRECT — only a test
// of behaviour does that, and those live beside each feature — but it can
// guarantee that nobody classified it by forgetting.

// access is what a route is allowed to be.
type access int

const (
	// public: deliberately reachable by anyone, including anonymous callers on
	// a multi-user instance. Every entry needs a reason.
	public access = iota
	// authed: requires a principal (or a token that stands in for one).
	authed
	// scoped: returns note content and must filter by space and reader list.
	scoped
	// admin: administers the instance.
	admin
)

var routeAccess = map[string]access{
	// --- public, on purpose ---
	"GET /api/admin/reads":           admin,  // who opened which restricted document
	"GET /api/admin/reads/anomalies": admin,  // bursts in that trail
	"GET /api/health":                public, // liveness; reveals counts, not content
	"GET /api/me":                    public, // the console asks this BEFORE signing in
	// Reports only what the caller already is — its own peer address, the
	// name it already sent, and which identity mechanisms are configured. It
	// deliberately returns no note content and no secret. Open because the
	// failure mode of identity configuration is silence, and the operator
	// debugging it has to be able to read this from the client that is
	// failing, which by definition is not being identified.
	"GET /api/identity":     public,
	"POST /api/auth/login":  public, // the sign-in route itself
	"POST /api/auth/logout": public,
	"POST /api/users":       public, // ONLY when no account exists; admin-gated after
	"GET /metrics":          public, // route classes and counts, never content
	// The published site. Public BY DESIGN and the only routes here that serve
	// note content to nobody in particular — which is why they exist only when
	// the operator turns publishing on, and serve only notes whose author
	// wrote publish: true. See publish.go.
	"GET /published":               public,
	"GET /published/{path...}":     public,
	"GET /api/published":           public,
	"GET /plugins/{name}/{rel...}": public, // static plugin assets, no note content

	// --- content: space + reader list ---
	"GET /api/notes":              scoped,
	"GET /api/notes/random":       scoped,
	"GET /api/notes/{path...}":    scoped,
	"PUT /api/notes/{path...}":    scoped,
	"POST /api/notes/{path...}":   scoped,
	"DELETE /api/notes/{path...}": scoped,
	"POST /api/notes":             scoped,
	"GET /api/search":             scoped,
	"GET /api/retrieve":           scoped,
	"GET /api/context":            scoped,
	"POST /api/ask":               scoped,
	"GET /api/graph":              scoped,
	"GET /api/tags":               scoped,
	"GET /api/tasks":              scoped,
	"GET /api/blocks":             scoped, // the lines inside notes
	"GET /api/bookmarks":          scoped, // resolves to notes the caller may read
	"GET /api/facts":              scoped,
	"GET /api/complete":           scoped,
	"GET /api/memory":             scoped,
	"GET /api/memory/export":      scoped, // every fact the caller may read
	"GET /api/memory/changes":     scoped, // fact text, so the same filter as recall
	"GET /api/memory/facets":      scoped, // scope names are drawn from facts
	"GET /api/memory/graph":       scoped, // entities and the facts behind them
	"POST /api/memory/search":     scoped, // recall, ranked by a supplied vector
	// Counts and configuration names, no note text — but it does describe the
	// deployment, so it is gated with the rest rather than public.
	"GET /api/doctor": scoped,
	// Token counts, costs and agent names — describes the deployment and who
	// used it, so it is gated with the rest rather than public.
	"GET /api/usage":        scoped,
	"GET /api/usage/agents": scoped,
	// Fact text on both sides of the disagreement, so the same filter as recall.
	"GET /api/memory/challenges": scoped,
	"GET /api/briefing":          scoped,
	// Counts per origin, filtered per caller. It says how much of the corpus
	// came from where — never which notes, so it cannot be used to enumerate
	// content the caller could not already list.
	"GET /api/trust": scoped,
	// The review queue: note paths and titles, so the same filter as any
	// listing. Bodies are never included.
	"GET /api/stale":               scoped,
	"GET /api/file/{path...}":      scoped,
	"GET /read":                    scoped,
	"GET /notes/{path...}":         scoped, // the HTML export, served without /api
	"POST /api/query":              scoped, // a query block lists notes
	"POST /api/template/render":    scoped, // a template pulls in a note body
	"GET /read/{path...}":          scoped,
	"GET /api/sync/manifest":       scoped,
	"POST /api/sync/pull":          scoped,
	"POST /api/sync/push":          scoped,
	"GET /api/export/vault":        scoped,
	"GET /api/aliases":             scoped,
	"GET /api/daily":               scoped,
	"GET /api/daily/dates":         scoped,
	"GET /api/canvas":              scoped,
	"GET /api/canvas/{path...}":    scoped,
	"PUT /api/canvas/{path...}":    scoped,
	"DELETE /api/canvas/{path...}": scoped,
	"GET /api/trash":               scoped,
	"GET /api/templates":           scoped,

	// --- writes and actions that need an account ---
	"POST /api/memory": authed,
	// Vouching for a pulled note rewrites its frontmatter, so it takes the
	// note's own write check inside the handler as well.
	"POST /api/stale/verify": authed,
	"POST /api/trust/vouch":  authed,
	// The joined audit trail: read paths, agent memory, and -- when the
	// vault is unlocked -- secret names and brokered URLs. Strictly more
	// revealing than any one of its three sources, so it is never public.
	"GET /api/timeline": authed,
	// An agent asking for a credential it has no grant for. It issues nothing
	// — a pending request confers no access — so it cannot be admin-gated, or
	// no agent could ever ask.
	"POST /api/secrets/requests": authed,
	// The asker collecting its own answer. This is the one read that can
	// return a live grant token, and the handler checks the grantee: a caller
	// gets its OWN request or a 404, never somebody else's.
	"GET /api/secrets/requests/{id}": authed,
	"POST /api/bookmarks":            authed,
	"DELETE /api/bookmarks":          authed,
	"POST /api/memory/batch":         authed,
	// Settling a challenge supersedes a fact either way, so it is a write.
	"POST /api/memory/challenge":    authed,
	"POST /api/memory/feedback":     authed,
	"POST /api/memory/consolidate":  authed,
	"PATCH /api/memory/entry":       authed,
	"DELETE /api/memory/entry":      authed,
	"POST /api/facts":               authed,
	"POST /api/capture":             authed,
	"POST /api/attach":              authed,
	"POST /api/audio":               authed,
	"POST /api/actions":             authed,
	"POST /api/templates":           authed,
	"POST /api/templates/apply":     authed,
	"POST /api/canvas":              authed,
	"POST /api/import/vault":        authed, // writes notes from an uploaded archive
	"POST /api/trash/{tid}/restore": authed,
	"DELETE /api/trash/{tid}":       authed,
	"POST /api/tags/rename":         authed,
	"GET /api/web/search":           authed,
	"POST /api/embed":               authed, // vectors from the local model; no note content
	"POST /api/web/fetch":           authed,
	"GET /api/keys":                 authed,
	"POST /api/keys":                authed,
	"DELETE /api/keys/{id}":         authed,
	"POST /api/auth/password":       authed,
	"GET /api/spaces":               authed, // filtered to what the caller may read
	"GET /api/plugins":              authed,
	"GET /api/sync/status":          authed,
	"POST /api/sync/now":            admin, // a whole-vault transfer to the peer
	"GET /api/crdt/doc/{path...}":   scoped,
	"POST /api/crdt/merge":          scoped,
	"GET /api/vault/status":         authed, // lock state only, never a name or value

	// --- instance administration ---
	"POST /api/vault/init":                       admin,
	"POST /api/vault/unlock":                     admin,
	"POST /api/vault/lock":                       admin,
	"POST /api/vault/change-passphrase":          admin,
	"GET /api/secrets":                           admin,
	"POST /api/secrets":                          admin,
	"DELETE /api/secrets/{name}":                 admin,
	"POST /api/secrets/{name}/grant":             admin,
	"POST /api/secrets/broker":                   authed, // the grant token IS the capability
	"GET /api/secrets/requests":                  admin,  // the human's approval queue
	"POST /api/secrets/requests/{id}/approve":    admin,  // mints a grant
	"POST /api/secrets/requests/{id}/deny":       admin,
	"GET /api/grants":                            admin,
	"DELETE /api/grants":                         admin,
	"DELETE /api/grants/{token}":                 admin,
	"GET /api/audit":                             admin,
	"POST /api/reindex":                          admin,
	"GET /api/settings":                          admin,
	"PUT /api/settings":                          admin,
	"GET /api/users":                             admin,
	"PUT /api/users/{id}":                        admin,
	"DELETE /api/users/{id}":                     admin,
	"GET /api/identities":                        admin,
	"POST /api/identities":                       admin,
	"DELETE /api/identities/{source}/{external}": admin,
	"POST /api/spaces":                           admin,
	"DELETE /api/spaces/{id}":                    admin,
	"GET /api/spaces/{id}/members":               admin,
	"POST /api/spaces/{id}/members":              admin,
	"DELETE /api/spaces/{id}/members/{user}":     admin,
	"GET /api/connectors":                        admin,
	"GET /api/connectors/kinds":                  admin,
	"POST /api/connectors":                       admin,
	"PUT /api/connectors/{id}":                   admin,
	"DELETE /api/connectors/{id}":                admin,
	"POST /api/connectors/{id}/run":              admin,
	"POST /api/plugins/scaffold":                 admin,
	"POST /api/plugins/{name}/enable":            admin,
}

var handleFuncRE = regexp.MustCompile(`mux\.HandleFunc\("([^"]+)"`)

// registeredRoutes reads every route this package registers.
func registeredRoutes(t *testing.T) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(".", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range handleFuncRE.FindAllStringSubmatch(string(body), -1) {
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func TestEveryRouteIsClassified(t *testing.T) {
	routes := registeredRoutes(t)
	if len(routes) < 50 {
		t.Fatalf("found only %d routes — did the registration form change?", len(routes))
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r] = true
		if _, ok := routeAccess[r]; !ok {
			t.Errorf("route %q has no access class.\n"+
				"Every route that returns or accepts content has to say who may call it. "+
				"Add it to routeAccess in this file — and if it is `public`, say why in a "+
				"comment, because that is the list an attacker reads first.", r)
		}
	}
	for r := range routeAccess {
		if !seen[r] {
			t.Errorf("routeAccess lists %q, which is no longer registered — stale entries "+
				"make this table lie about what is reachable", r)
		}
	}
}

// The public list is the one worth reading twice: everything on it is reachable
// by anyone who can open the port on a multi-user instance.
func TestThePublicSurfaceIsSmallAndDeliberate(t *testing.T) {
	var pub []string
	for r, a := range routeAccess {
		if a == public {
			pub = append(pub, r)
		}
	}
	sort.Strings(pub)
	// Raised from 8 to 11 for the published site, deliberately: three routes
	// that serve note content to nobody in particular. What makes that
	// defensible is that they do not exist unless an operator turns publishing
	// on, and that they serve only notes whose author wrote publish: true —
	// both of which are tested in publish_test.go rather than asserted here.
	if len(pub) > 11 {
		t.Errorf("the anonymous surface has grown to %d routes: %v\n"+
			"Each one is reachable without an account; if that is intended, raise this "+
			"bound deliberately.", len(pub), pub)
	}
	for _, r := range pub {
		if strings.Contains(r, "/notes") || strings.Contains(r, "/search") ||
			strings.Contains(r, "/sync") || strings.Contains(r, "/read") ||
			strings.Contains(r, "/export") {
			t.Errorf("%q is classified public but looks like a content route", r)
		}
	}
}

// Every non-public route must actually REFUSE an anonymous caller.
//
// The table above records a decision; it cannot tell whether the code honours
// it. That gap is not hypothetical — it hid three holes in the trash surface
// for as long as the table existed. GET /api/trash was labelled `scoped` while
// its handler took the request as `_`, which is the bug in one character: a
// handler that never looks at who is asking cannot be filtering. It answered
// anyone with every deleted note's path and title, and restore and purge let
// anyone move a note back into a space they cannot write or destroy its last
// copy.
//
// So this drives the real mux. Accounts exist, nobody is signed in, and a
// success is a failure. It does not check that the FILTERING is right — that
// needs a second principal and lives beside each feature — but no route can
// ever again be reachable by an anonymous caller merely because someone wrote
// a label next to it.
func TestNonPublicRoutesRefuseAnonymousCallers(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "alice", "admin") // accounts exist ⇒ multi-user rules apply

	// The probe targets have to EXIST, or a route 404s on the way to the
	// branch worth testing and reports itself guarded. Removing the write
	// check from POST /api/facts did not fail this test until these were here.
	for _, p := range []string{"probe/note.md", "memory/probe.md", "templates/probe.md"} {
		if w := asKey(t, h, adminKey, "POST", "/api/notes", map[string]any{
			"path": p, "body": "# Probe\n\nbody"}); w.Code != http.StatusCreated {
			t.Fatalf("seeding %s = %d %s", p, w.Code, w.Body)
		}
	}

	for route, class := range routeAccess {
		if class == public {
			continue
		}
		method, pattern, ok := strings.Cut(route, " ")
		if !ok {
			t.Fatalf("route %q is not \"METHOD /path\"", route)
		}
		// A `scoped` READ may answer an anonymous caller — with nothing in it.
		// Notes and retrieval stay reachable on a trusted network by design;
		// what must not happen is content coming back, and that is checked by
		// the content sweeps in multiuser_test.go with a real second principal.
		// A scoped WRITE is a different matter: there is no such thing as an
		// anonymous write once accounts exist.
		if class == scoped && !writesContent(method, route) {
			continue
		}
		t.Run(route, func(t *testing.T) {
			code := statusAnonymously(t, h, method, fillWildcards(pattern), bodyFor(route))
			if code >= 200 && code < 300 {
				t.Errorf("%s answered an anonymous caller with %d.\n"+
					"It is classified %s, so it must refuse one. A label is not a check: "+
					"look at whether the handler ever reads the request's principal.",
					route, code, className(class))
			}
		})
	}
}

// writesContent reports whether a route changes the vault. POST is not a
// reliable signal on its own: /api/ask and /api/query are reads that take a
// body, so they are named here rather than guessed at.
func writesContent(method, route string) bool {
	switch route {
	case "POST /api/ask", "POST /api/query":
		return false
	}
	return method == "PUT" || method == "POST" || method == "DELETE" || method == "PATCH"
}

func className(a access) string {
	switch a {
	case authed:
		return "authed"
	case scoped:
		return "scoped"
	case admin:
		return "admin"
	}
	return "public"
}

// fillWildcards turns a mux pattern into a concrete path. The values only have
// to be well-formed: a refused request never reaches the thing they name.
func fillWildcards(pattern string) string {
	r := strings.NewReplacer(
		"{path...}", "probe/note.md",
		"{rel...}", "probe.js",
		"{name}", "probe",
		"{id}", "probe",
		"{tid}", "probe",
		"{version}", "1",
		"{key}", "probe",
		"{token}", "probe",
	)
	return r.Replace(pattern)
}

// statusAnonymously performs one request with no credentials, failing the test
// if the handler never returns — a route that hangs an anonymous caller is its
// own kind of unguarded.
// bodyFor supplies a request body good enough to get PAST validation.
//
// An empty {} is not a probe, it is a typo check: POST /api/facts and
// POST /api/memory/consolidate both answered 400 "field required" to an
// anonymous caller and so looked guarded, while the branch behind that
// validation wrote to any note in the vault. A probe that never reaches the
// dangerous branch tests nothing, and — worse — reports that it did.
func bodyFor(route string) string {
	switch route {
	case "POST /api/facts":
		return `{"note":"probe/note.md","key":"k","value":"v"}`
	case "POST /api/memory/consolidate":
		return `{"path":"memory/probe.md"}`
	case "POST /api/templates/apply":
		return `{"template":"templates/probe.md","title":"probe"}`
	case "POST /api/capture":
		return `{"text":"probe","title":"probe"}`
	case "POST /api/query":
		return `{"block":"tag: probe"}`
	case "POST /api/ask":
		return `{"q":"probe"}`
	case "POST /api/vault/change-passphrase":
		return `{"old":"probe-old","new":"probe-new"}`
	}
	return "{}"
}

func statusAnonymously(t *testing.T, h http.Handler, method, path, body string) int {
	t.Helper()
	done := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		done <- w.Code
	}()
	select {
	case code := <-done:
		return code
	case <-time.After(10 * time.Second):
		t.Fatalf("%s %s never answered an anonymous caller", method, path)
		return 0
	}
}
