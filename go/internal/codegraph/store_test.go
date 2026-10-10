package codegraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return NewStore(database)
}

// writeRepo lays files out under a fresh directory and returns its absolute
// path. Keys are slash-separated relative paths.
func writeRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mustIndex(t *testing.T, s *Store, root string) Stats {
	t.Helper()
	st, err := s.Index(root)
	if err != nil {
		t.Fatalf("Index(%s): %v", root, err)
	}
	return st
}

func baseRepo() map[string]string {
	return map[string]string{
		"go.mod":              "module example.com/store\n",
		"store/store.go":      goFixture,
		"app/repo.py":         pyFixture,
		"web/circle.ts":       tsFixture,
		"web/widget.js":       jsFixture,
		"notes/README.md":     "# not code\n",
		"vendor/dep/dep.go":   "package dep\n\nfunc Vendored() {}\n",
		"node_modules/x/i.js": "export function Leaked() {}\n",
		".git/hooks/h.py":     "def hook(): pass\n",
		"gen/ignored.go":      "package gen\n\nfunc Generated() {}\n",
		".gitignore":          "# generated code\ngen/\n",
	}
}

func TestIndexFixtureRepo(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, baseRepo())
	st := mustIndex(t, s, root)
	if st.Indexed != 4 || st.Failed != 0 {
		t.Fatalf("stats = %+v, want 4 source files indexed and none failed", st)
	}
	if st.Symbols == 0 || st.Edges == 0 {
		t.Fatalf("stats = %+v, expected symbols and edges", st)
	}
	// Skipped trees never reach the index, even when a name would match.
	for _, name := range []string{"Vendored", "Leaked", "hook", "Generated"} {
		hits, err := s.Symbols(name, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 0 {
			t.Errorf("%s was indexed from a skipped path: %+v", name, hits)
		}
	}
	// A non-source file is not a source file, whatever its content.
	if hits, _ := s.Symbols("README", "", "", 10); len(hits) != 0 {
		t.Errorf("markdown was indexed: %+v", hits)
	}
}

func TestIndexSkipsFilesOverTheSizeCap(t *testing.T) {
	s := openStore(t)
	s.MaxFileBytes = 200
	big := "package big\n\n" + strings.Repeat("// padding padding padding\n", 20) + "func Huge() {}\n"
	root := writeRepo(t, map[string]string{
		"small.go": "package small\n\nfunc Tiny() {}\n",
		"big.go":   big,
	})
	st := mustIndex(t, s, root)
	if st.TooLarge != 1 || st.Indexed != 1 {
		t.Fatalf("stats = %+v, want one file too large and one indexed", st)
	}
	if hits, _ := s.Symbols("Huge", "", "", 10); len(hits) != 0 {
		t.Error("a file over the size cap was parsed")
	}
	if hits, _ := s.Symbols("Tiny", "", "", 10); len(hits) != 1 {
		t.Error("a file under the cap was not indexed")
	}
}

func TestIndexIsIncremental(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, baseRepo())
	first := mustIndex(t, s, root)

	// Nothing changed: every file is recognised by its hash and not re-parsed.
	again := mustIndex(t, s, root)
	if again.Indexed != 0 || again.Unchanged != first.Indexed || again.Removed != 0 {
		t.Fatalf("second run = %+v, want all %d unchanged", again, first.Indexed)
	}
	if again.Symbols != first.Symbols || again.Edges != first.Edges {
		t.Fatalf("totals moved without a change: %+v vs %+v", again, first)
	}

	// One file edited: only that file is parsed, and its new symbol appears.
	edited := strings.Replace(goFixture, "func helper(v string) error {", "func renamedHelper(v string) error {", 1)
	if err := os.WriteFile(filepath.Join(root, "store", "store.go"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	third := mustIndex(t, s, root)
	if third.Indexed != 1 || third.Unchanged != first.Indexed-1 {
		t.Fatalf("after one edit = %+v, want 1 indexed", third)
	}
	if hits, _ := s.Symbols("renamedHelper", "", "", 10); len(hits) != 1 {
		t.Errorf("edited symbol not visible: %+v", hits)
	}
	// widget.js has its own helper, so scope the check to the edited file.
	hits, _ := s.Symbols("helper", KindFunc, root, 10)
	for _, h := range hits {
		if h.Path == "store/store.go" {
			t.Errorf("old symbol survived the edit: %+v", h)
		}
	}
}

func TestIndexRemovesDeletedFiles(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, map[string]string{
		"a.go": "package a\n\nfunc Alpha() {}\n",
		"b.go": "package a\n\nfunc Beta() {}\n",
	})
	mustIndex(t, s, root)
	if err := os.Remove(filepath.Join(root, "b.go")); err != nil {
		t.Fatal(err)
	}
	st := mustIndex(t, s, root)
	if st.Removed != 1 {
		t.Fatalf("stats = %+v, want one removed", st)
	}
	if hits, _ := s.Symbols("Beta", "", "", 10); len(hits) != 0 {
		t.Errorf("symbols of a deleted file are still answered: %+v", hits)
	}
	if out, _ := s.Outline("b.go"); len(out) != 0 {
		t.Errorf("outline of a deleted file: %+v", out)
	}
	if hits, _ := s.Symbols("Alpha", "", "", 10); len(hits) != 1 {
		t.Error("a surviving file lost its symbols")
	}
}

func TestIndexRecordsAFileThatDoesNotParse(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, map[string]string{
		"ok.go":     "package ok\n\nfunc Fine() {}\n",
		"broken.go": "package broken\n\nfunc (\n",
	})
	st := mustIndex(t, s, root)
	if st.Failed != 1 {
		t.Fatalf("stats = %+v, want one failed", st)
	}
	out, err := s.Outline("broken.go")
	if err != nil || len(out) != 1 {
		t.Fatalf("outline = %+v, %v", out, err)
	}
	if out[0].Error == "" || len(out[0].Symbols) != 0 {
		t.Errorf("a broken file should be visible with its error and no symbols: %+v", out[0])
	}
}

func TestIndexRejectsRelativeAndMissingRoots(t *testing.T) {
	s := openStore(t)
	if _, err := s.Index("relative/dir"); err == nil {
		t.Error("a relative root was accepted")
	}
	if _, err := s.Index(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Error("a missing root was accepted")
	}
}

func TestTwoRootsDoNotOverwriteEachOther(t *testing.T) {
	s := openStore(t)
	a := writeRepo(t, map[string]string{"main.go": "package a\n\nfunc Shared() {}\n"})
	b := writeRepo(t, map[string]string{"main.go": "package b\n\nfunc Shared() {}\n"})
	mustIndex(t, s, a)
	mustIndex(t, s, b)
	hits, err := s.Symbols("Shared", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("two checkouts with the same relative path gave %d hits, want 2", len(hits))
	}
	// Narrowing by root returns only that checkout's definition.
	only, _ := s.Symbols("Shared", "", a, 10)
	if len(only) != 1 || only[0].Root != a {
		t.Fatalf("root filter = %+v", only)
	}
}

func TestSymbolLookupByNameQualifiedNameAndKind(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, baseRepo())
	mustIndex(t, s, root)

	hits, err := s.Symbols("Save", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("Save not found")
	}
	var goMethod *Hit
	for i := range hits {
		if hits[i].Qualified == "Store.Save" {
			goMethod = &hits[i]
		}
	}
	if goMethod == nil || goMethod.Kind != KindMethod || !strings.HasSuffix(goMethod.Path, "store/store.go") {
		t.Fatalf("Store.Save not among hits: %+v", hits)
	}
	if goMethod.Line != lineOf(goFixture, "func (s *Store) Save(") {
		t.Errorf("Save line = %d", goMethod.Line)
	}

	qual, _ := s.Symbols("Store.Save", "", "", 10)
	if len(qual) != 1 || qual[0].Qualified != "Store.Save" {
		t.Errorf("qualified lookup = %+v", qual)
	}
	structs, _ := s.Symbols("Store", KindStruct, "", 10)
	if len(structs) != 1 || structs[0].Kind != KindStruct {
		t.Errorf("kind filter = %+v", structs)
	}
	if none, _ := s.Symbols("NoSuchName", "", "", 10); len(none) != 0 {
		t.Errorf("unknown name returned %+v", none)
	}
}

func TestCallersByName(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, baseRepo())
	mustIndex(t, s, root)

	// Save is called from Caller on a variable, so it has a qualifier "st"
	// and a bare-name query still finds it.
	all, err := s.Callers("Save", 50)
	if err != nil {
		t.Fatal(err)
	}
	var fromCaller *Caller
	for i := range all {
		if all[i].Caller == "Caller" && all[i].Callee == "Save" {
			fromCaller = &all[i]
		}
	}
	if fromCaller == nil {
		t.Fatalf("no caller of Save recorded: %+v", all)
	}
	if fromCaller.Line != lineOf(baseRepoFile("store/store.go"), "_ = st.Save(") {
		t.Errorf("call line = %d", fromCaller.Line)
	}

	// helper is called from Store.Save, qualified by the method's name.
	helperCallers, _ := s.Callers("helper", 50)
	if len(helperCallers) == 0 || helperCallers[0].Caller != "Store.Save" {
		t.Errorf("callers of helper = %+v", helperCallers)
	}

	// A dotted name narrows to calls written through that qualifier.
	pkgCalls, _ := s.Callers("fmt2.Println", 50)
	if len(pkgCalls) != 1 || pkgCalls[0].Caller != "New" {
		t.Errorf("fmt2.Println callers = %+v", pkgCalls)
	}
	none, _ := s.Callers("nosuchfunc", 50)
	if len(none) != 0 {
		t.Errorf("unknown callee returned %+v", none)
	}
}

// baseRepoFile returns the content of a file in baseRepo by its relative path.
func baseRepoFile(rel string) string { return baseRepo()[rel] }

func TestOutlineForRelativeAndAbsolutePaths(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, baseRepo())
	mustIndex(t, s, root)

	rel, err := s.Outline("app/repo.py")
	if err != nil || len(rel) != 1 {
		t.Fatalf("relative outline = %+v, %v", rel, err)
	}
	abs, err := s.Outline(filepath.Join(root, "app", "repo.py"))
	if err != nil || len(abs) != 1 {
		t.Fatalf("absolute outline = %+v, %v", abs, err)
	}
	fo := abs[0]
	if fo.Lang != LangPython || fo.Root != root {
		t.Errorf("outline header = %+v", fo)
	}
	// Symbols come back in source order, so an agent can read the file's shape.
	for i := 1; i < len(fo.Symbols); i++ {
		if fo.Symbols[i].Line < fo.Symbols[i-1].Line {
			t.Errorf("outline not in line order: %+v", fo.Symbols)
		}
	}
	if len(fo.Imports) != 3 {
		t.Errorf("imports = %+v", fo.Imports)
	}
	if empty, _ := s.Outline("app/missing.py"); len(empty) != 0 {
		t.Errorf("a file that is not indexed has an outline: %+v", empty)
	}
}
