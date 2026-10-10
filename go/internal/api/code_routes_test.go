package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/codegraph"
)

const codeRepoStore = `package store

// Save writes the row.
func (s *Store) Save(row string) error {
	return persist(row)
}

func persist(row string) error { return nil }

func Run() {
	st := &Store{}
	_ = st.Save("x")
	_ = persist("y")
}

type Store struct{}
`

const codeRepoPy = `import os

class Worker:
    def start(self):
        return os.getcwd()
`

// codeRepo writes a small repository under a fresh directory and returns it.
func codeRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"store/store.go": codeRepoStore, "jobs/worker.py": codeRepoPy}
	for rel, body := range files {
		abs := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// codeServer is a server with the vault and one extra code root allowed, and
// an admin and a member so the gate can be exercised.
func codeServer(t *testing.T) (s *Server, h http.Handler, admin, member string, extra string) {
	t.Helper()
	s, h = testServer(t)
	admin = makeUser(t, s, h, "", "root", "admin")
	member = makeUser(t, s, h, admin, "bob", "member")
	extra = codeRepo(t)
	s.CodeRoots = []string{extra}
	return s, h, admin, member, extra
}

// indexAs posts an index request as key and returns the recorder.
func indexAs(t *testing.T, h http.Handler, key, path string) *httptest.ResponseRecorder {
	t.Helper()
	return asKey(t, h, key, "POST", "/api/code/index", map[string]string{"path": path})
}

func TestCodeRoutesRefuseMembersAndAnonymous(t *testing.T) {
	_, h, admin, member, extra := codeServer(t)
	if r := indexAs(t, h, admin, extra); r.Code != http.StatusOK {
		t.Fatalf("admin index: %d %s", r.Code, r.Body)
	}
	paths := []string{
		"/api/code/symbol?name=Save",
		"/api/code/callers?name=persist",
		"/api/code/outline?file=store/store.go",
	}
	for _, who := range []string{member, ""} {
		for _, p := range paths {
			w := asKey(t, h, who, "GET", p, nil)
			if w.Code == http.StatusOK {
				t.Errorf("%q as %q answered 200: %s", p, who, w.Body)
			}
			if strings.Contains(w.Body.String(), "persist") {
				t.Errorf("%q leaked symbol data to a non-admin: %s", p, w.Body)
			}
		}
		w := asKey(t, h, who, "POST", "/api/code/index", map[string]string{"path": extra})
		if w.Code == http.StatusOK {
			t.Errorf("indexing as %q answered 200", who)
		}
	}
}

func TestCodeIndexPathAllowlist(t *testing.T) {
	s, h, admin, _, extra := codeServer(t)
	outside := t.TempDir()

	// A sibling that shares a prefix with an allowed root is a different
	// directory, not a subdirectory of it.
	sibling := extra + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	// A symlink inside an allowed root that points outside it must not
	// launder the path past the check.
	link := filepath.Join(extra, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want int
	}{
		{"vault root is allowed", s.Vault.Root, http.StatusOK},
		{"subdirectory of an allowed code root", filepath.Join(extra, "store"), http.StatusOK},
		{"sibling sharing a prefix", sibling, http.StatusForbidden},
		{"filesystem root", "/", http.StatusForbidden},
		{"outside directory", outside, http.StatusForbidden},
		{"symlink escaping the root", link, http.StatusForbidden},
		{"relative path", "store", http.StatusBadRequest},
		{"empty path", "", http.StatusBadRequest},
		{"missing directory", filepath.Join(extra, "absent"), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := indexAs(t, h, admin, c.path)
			if r.Code != c.want {
				t.Fatalf("index %q: %d, want %d: %s", c.path, r.Code, c.want, r.Body)
			}
		})
	}

	// A file is not a directory, and a body that is not JSON is refused.
	w := asKey(t, h, admin, "POST", "/api/code/index", map[string]string{"path": filepath.Join(extra, "store", "store.go")})
	if w.Code != http.StatusBadRequest {
		t.Errorf("indexing a file: %d", w.Code)
	}
	bad := httptest.NewRequest("POST", "/api/code/index", strings.NewReader("not json"))
	bad.Header.Set("Authorization", "Bearer "+admin)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, bad)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-JSON body: %d", rec.Code)
	}
}

func TestCodeGraphRoundTripAndIncrementalReindex(t *testing.T) {
	_, h, admin, _, extra := codeServer(t)

	if first := indexAs(t, h, admin, extra); first.Code != http.StatusOK {
		t.Fatalf("index: %d %s", first.Code, first.Body)
	}
	var st codegraph.Stats
	decode(t, asKey(t, h, admin, "POST", "/api/code/index", map[string]string{"path": extra}), &st)
	if st.Unchanged != st.Files || st.Indexed != 0 {
		t.Fatalf("re-running on an unchanged repo = %+v, want every file unchanged", st)
	}

	// Where is Save defined: the method, with its line, found by bare name and
	// by qualified name.
	var sym struct {
		Symbols []codegraph.Hit `json:"symbols"`
	}
	decode(t, asKey(t, h, admin, "GET", "/api/code/symbol?name=Save&kind=method", nil), &sym)
	if len(sym.Symbols) != 1 || sym.Symbols[0].Qualified != "Store.Save" || sym.Symbols[0].Line != 4 {
		t.Fatalf("symbol Save = %+v", sym.Symbols)
	}
	decode(t, asKey(t, h, admin, "GET", "/api/code/symbol?name=Store.Save", nil), &sym)
	if len(sym.Symbols) != 1 {
		t.Fatalf("qualified lookup = %+v", sym.Symbols)
	}

	// Who calls persist: the method and Run, by name.
	var calls struct {
		Callers []codegraph.Caller `json:"callers"`
		Note    string             `json:"note"`
	}
	decode(t, asKey(t, h, admin, "GET", "/api/code/callers?name=persist", nil), &calls)
	callers := map[string]bool{}
	for _, c := range calls.Callers {
		callers[c.Caller] = true
	}
	if !callers["Store.Save"] || !callers["Run"] {
		t.Fatalf("callers of persist = %+v", calls.Callers)
	}
	if !strings.Contains(calls.Note, "approximate") {
		t.Errorf("callers response does not say it is approximate: %q", calls.Note)
	}

	// The outline of the Python file lists its class and method, and its import.
	var out struct {
		Files []codegraph.FileOutline `json:"files"`
	}
	decode(t, asKey(t, h, admin, "GET", "/api/code/outline?file="+filepath.Join(extra, "jobs", "worker.py"), nil), &out)
	if len(out.Files) != 1 || len(out.Files[0].Symbols) != 2 || len(out.Files[0].Imports) != 1 {
		t.Fatalf("outline = %+v", out.Files)
	}

	// Editing one file re-parses only that file, and the new name is answerable.
	edited := strings.Replace(codeRepoPy, "def start", "def begin", 1)
	if err := os.WriteFile(filepath.Join(extra, "jobs", "worker.py"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	var re codegraph.Stats
	decode(t, asKey(t, h, admin, "POST", "/api/code/index", map[string]string{"path": extra}), &re)
	if re.Indexed != 1 {
		t.Fatalf("after one edit, indexed = %d, want 1", re.Indexed)
	}
	decode(t, asKey(t, h, admin, "GET", "/api/code/symbol?name=begin", nil), &sym)
	if len(sym.Symbols) != 1 {
		t.Fatalf("renamed method not found: %+v", sym.Symbols)
	}

	// Bad queries are refused, not answered with everything.
	for _, p := range []string{"/api/code/symbol", "/api/code/symbol?name=x&kind=bogus", "/api/code/callers", "/api/code/outline"} {
		if w := asKey(t, h, admin, "GET", p, nil); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d, want 400", p, w.Code)
		}
	}
}
