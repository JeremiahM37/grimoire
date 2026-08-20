package api

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
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
	"GET /api/health":              public, // liveness; reveals counts, not content
	"GET /api/me":                  public, // the console asks this BEFORE signing in
	"POST /api/auth/login":         public, // the sign-in route itself
	"POST /api/auth/logout":        public,
	"POST /api/users":              public, // ONLY when no account exists; admin-gated after
	"GET /metrics":                 public, // route classes and counts, never content
	"GET /plugins/{name}/{rel...}": public, // static plugin assets, no note content

	// --- content: space + reader list ---
	"GET /api/notes":               scoped,
	"GET /api/notes/random":        scoped,
	"GET /api/notes/{path...}":     scoped,
	"PUT /api/notes/{path...}":     scoped,
	"POST /api/notes/{path...}":    scoped,
	"DELETE /api/notes/{path...}":  scoped,
	"POST /api/notes":              scoped,
	"GET /api/search":              scoped,
	"GET /api/retrieve":            scoped,
	"GET /api/context":             scoped,
	"POST /api/ask":                scoped,
	"GET /api/graph":               scoped,
	"GET /api/tags":                scoped,
	"GET /api/tasks":               scoped,
	"GET /api/facts":               scoped,
	"GET /api/complete":            scoped,
	"GET /api/memory":              scoped,
	"GET /api/briefing":            scoped,
	"GET /api/file/{path...}":      scoped,
	"GET /read":                    scoped,
	"GET /notes/{path...}":         scoped, // the HTML export, served without /api
	"POST /api/query":              scoped, // a query block lists notes
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
	"POST /api/memory":              authed,
	"POST /api/memory/consolidate":  authed,
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
	"POST /api/web/fetch":           authed,
	"GET /api/keys":                 authed,
	"POST /api/keys":                authed,
	"DELETE /api/keys/{id}":         authed,
	"POST /api/auth/password":       authed,
	"GET /api/spaces":               authed, // filtered to what the caller may read
	"GET /api/plugins":              authed,
	"GET /api/sync/status":          authed,
	"POST /api/sync/now":            authed,
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
	if len(pub) > 8 {
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
