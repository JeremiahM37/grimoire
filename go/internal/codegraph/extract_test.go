package codegraph

import (
	"strings"
	"testing"
)

func findSym(t *testing.T, g FileGraph, qualified string) Symbol {
	t.Helper()
	for _, s := range g.Symbols {
		if s.Qualified() == qualified {
			return s
		}
	}
	t.Fatalf("no symbol %q in %+v", qualified, g.Symbols)
	return Symbol{}
}

func findEdge(t *testing.T, g FileGraph, kind, callee, caller string) Edge {
	t.Helper()
	for _, e := range g.Edges {
		if e.Kind == kind && e.Callee == callee && e.Caller == caller {
			return e
		}
	}
	t.Fatalf("no %s edge to %q from %q in %+v", kind, callee, caller, g.Edges)
	return Edge{}
}

func TestLangFor(t *testing.T) {
	cases := map[string]string{
		"a.go": LangGo, "x.py": LangPython, "a.ts": LangTypeScript, "a.tsx": LangTypeScript,
		"a.js": LangJavaScript, "a.mjs": LangJavaScript, "a.jsx": LangJavaScript,
		"a.md": "", "a.json": "", "Makefile": "",
	}
	for name, want := range cases {
		if got := LangFor(name); got != want {
			t.Errorf("LangFor(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestExtractGoDeclarations(t *testing.T) {
	g, err := Extract("store.go", []byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	if g.Lang != LangGo {
		t.Fatalf("lang = %q", g.Lang)
	}
	want := map[string]struct {
		kind string
		line string // a substring of the declaration line
	}{
		"New":         {KindFunc, "func New("},
		"Store":       {KindStruct, "type Store struct"},
		"Saver":       {KindInterface, "type Saver interface"},
		"Pair":        {KindStruct, "type Pair[K"},
		"MaxRows":     {KindConst, "MaxRows = 10"},
		"defaultName": {KindVar, "defaultName = "},
		"Store.Save":  {KindMethod, "func (s *Store) Save("},
		"Pair.Key":    {KindMethod, "func (p *Pair[K, V]) Key("},
		"helper":      {KindFunc, "func helper("},
		"Caller":      {KindFunc, "func Caller("},
	}
	for q, w := range want {
		sym := findSym(t, g, q)
		if sym.Kind != w.kind {
			t.Errorf("%s kind = %q, want %q", q, sym.Kind, w.kind)
		}
		if line := lineOf(goFixture, w.line); sym.Line != line {
			t.Errorf("%s line = %d, want %d", q, sym.Line, line)
		}
		if sym.EndLine < sym.Line {
			t.Errorf("%s end line %d before start %d", q, sym.EndLine, sym.Line)
		}
	}
	// A method's scope is its receiver type with the pointer and type
	// parameters stripped, so Pair[K, V] is Pair.
	if sym := findSym(t, g, "Pair.Key"); sym.Scope != "Pair" || sym.Name != "Key" {
		t.Errorf("Pair.Key scope/name = %q/%q", sym.Scope, sym.Name)
	}
}

func TestExtractGoCallsAndImports(t *testing.T) {
	g, err := Extract("store.go", []byte(goFixture))
	if err != nil {
		t.Fatal(err)
	}
	// Imports keep their path, and an alias goes in Qual.
	imp := findEdge(t, g, EdgeImport, "fmt", "")
	if imp.Qual != "fmt2" || imp.Line != lineOf(goFixture, `fmt2 "fmt"`) {
		t.Errorf("aliased import = %+v", imp)
	}
	findEdge(t, g, EdgeImport, "context", "")
	findEdge(t, g, EdgeImport, "strings", "")

	// A bare call is recorded with no qualifier; a selector call with the
	// identifier it was selected from.
	helperCall := findEdge(t, g, EdgeCall, "helper", "Store.Save")
	if helperCall.Qual != "" || helperCall.Line != lineOf(goFixture, "return helper(s.name)") {
		t.Errorf("helper call = %+v", helperCall)
	}
	saveCall := findEdge(t, g, EdgeCall, "Save", "Caller")
	if saveCall.Qual != "st" {
		t.Errorf("st.Save() qual = %q, want st", saveCall.Qual)
	}
	// Qualified package calls keep the package name, so pkg.Func is findable.
	fmtCall := findEdge(t, g, EdgeCall, "Println", "New")
	if fmtCall.Qual != "fmt2" {
		t.Errorf("fmt2.Println qual = %q", fmtCall.Qual)
	}
	// A generic instantiation is still a call to the function it names.
	if !hasEdge(g, EdgeCall, "Background", "Caller") {
		t.Error("context.Background() inside Caller was not recorded")
	}
	// Builtins are dropped: len is not an edge to anything in the repository.
	if hasEdge(g, EdgeCall, "len", "helper") {
		t.Error("builtin len() was recorded as a call")
	}
}

func hasEdge(g FileGraph, kind, callee, caller string) bool {
	for _, e := range g.Edges {
		if e.Kind == kind && e.Callee == callee && e.Caller == caller {
			return true
		}
	}
	return false
}

func TestExtractGoParseErrorIsAnError(t *testing.T) {
	if _, err := Extract("broken.go", []byte("package x\n\nfunc (\n")); err == nil {
		t.Fatal("a file that does not parse must be an error, not an empty graph")
	}
}

const goPackageLevelCall = `package p

var table = build()

func build() int { return 1 }
`

func TestExtractGoPackageLevelCallHasNoCaller(t *testing.T) {
	g, err := Extract("p.go", []byte(goPackageLevelCall))
	if err != nil {
		t.Fatal(err)
	}
	findEdge(t, g, EdgeCall, "build", "")
}

func TestExtractPython(t *testing.T) {
	g, err := Extract("repo.py", []byte(pyFixture))
	if err != nil {
		t.Fatal(err)
	}
	if g.Lang != LangPython {
		t.Fatalf("lang = %q", g.Lang)
	}
	repo := findSym(t, g, "Repo")
	if repo.Kind != KindClass || repo.Line != lineOf(pyFixture, "class Repo") {
		t.Errorf("Repo = %+v", repo)
	}
	if repo.EndLine < lineOf(pyFixture, "async def load") {
		t.Errorf("Repo ends at %d, before its last method", repo.EndLine)
	}
	init := findSym(t, g, "Repo.__init__")
	if init.Kind != KindMethod {
		t.Errorf("__init__ kind = %q, want method", init.Kind)
	}
	// async def is a method too, and the indentation keeps it in the class.
	if load := findSym(t, g, "Repo.load"); load.Kind != KindMethod {
		t.Errorf("load kind = %q", load.Kind)
	}
	top := findSym(t, g, "top_level")
	if top.Kind != KindFunc || top.Scope != "" {
		t.Errorf("top_level = %+v", top)
	}
	// A def nested in a function is a function, not a method of anything.
	inner := findSym(t, g, "inner")
	if inner.Kind != KindFunc || inner.Scope != "" {
		t.Errorf("inner = %+v", inner)
	}

	findEdge(t, g, EdgeImport, "os", "")
	js := findEdge(t, g, EdgeImport, "json", "")
	if js.Qual != "js" {
		t.Errorf("json as js: qual = %q", js.Qual)
	}
	findEdge(t, g, EdgeImport, "pathlib", "")
	if g.Edges == nil || len(g.Edges) != 3 {
		t.Errorf("python imports = %d, want 3", len(g.Edges))
	}
}

func TestExtractScriptTypeScript(t *testing.T) {
	g, err := Extract("circle.ts", []byte(tsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if g.Lang != LangTypeScript {
		t.Fatalf("lang = %q", g.Lang)
	}
	checks := map[string]string{
		"Shape":       KindInterface,
		"Id":          KindType,
		"Circle":      KindClass,
		"Circle.area": KindMethod,
		"makeCircle":  KindFunc,
		"double":      KindFunc, // an arrow assigned to const is a function
		"LIMIT":       KindConst,
	}
	for q, kind := range checks {
		if sym := findSym(t, g, q); sym.Kind != kind {
			t.Errorf("%s kind = %q, want %q", q, sym.Kind, kind)
		}
	}
	if circle := findSym(t, g, "Circle"); circle.EndLine < lineOf(tsFixture, "return Math.PI") {
		t.Errorf("Circle ends at %d, before its method body", circle.EndLine)
	}
	// The closing brace of a class sits at the same indent as its opener and
	// belongs to it.
	if circle := findSym(t, g, "Circle"); !strings.Contains(lineText(tsFixture, circle.EndLine), "}") {
		t.Errorf("Circle end line %d is %q, want the closing brace", circle.EndLine, lineText(tsFixture, circle.EndLine))
	}
	findEdge(t, g, EdgeImport, "./client", "")
	findEdge(t, g, EdgeImport, "./opts", "")
	findEdge(t, g, EdgeImport, "./thing", "")
	findEdge(t, g, EdgeImport, "fs", "")
}

func TestExtractScriptJavaScript(t *testing.T) {
	g, err := Extract("widget.js", []byte(jsFixture))
	if err != nil {
		t.Fatal(err)
	}
	if g.Lang != LangJavaScript {
		t.Fatalf("lang = %q", g.Lang)
	}
	if sym := findSym(t, g, "render"); sym.Kind != KindFunc || sym.Line != lineOf(jsFixture, "export function render") {
		t.Errorf("render = %+v", sym)
	}
	if sym := findSym(t, g, "Widget.mount"); sym.Kind != KindMethod {
		t.Errorf("Widget.mount = %+v", sym)
	}
	// `if (...) {` inside a method looks like a method declaration; it is not.
	for _, s := range g.Symbols {
		if s.Name == "if" {
			t.Errorf("control flow recorded as a symbol: %+v", s)
		}
	}
	if sym := findSym(t, g, "helper"); sym.Kind != KindFunc {
		t.Errorf("helper = %+v, want a function (function expression)", sym)
	}
	findEdge(t, g, EdgeImport, "path", "")
	findEdge(t, g, EdgeImport, "./x.js", "")
}

func lineText(src string, n int) string {
	lines := strings.Split(src, "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

func TestExtractUnsupportedLanguage(t *testing.T) {
	if _, err := Extract("notes.md", []byte("# hi")); err == nil {
		t.Fatal("markdown has no extractor and must say so")
	}
}
