package codegraph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// callAt returns the call edge written on the line that contains sub. Line
// numbers identify a call better than its callee name, because one fixture
// often calls the same name more than once.
func callAt(t *testing.T, g FileGraph, src, sub string) Edge {
	t.Helper()
	want := lineOf(src, sub)
	for _, e := range g.Edges {
		if e.Kind == EdgeCall && e.Line == want {
			return e
		}
	}
	t.Fatalf("no call edge on line %d (%q)", want, sub)
	return Edge{}
}

func expectCall(t *testing.T, e Edge, target, conf string) {
	t.Helper()
	if e.Target != target || e.Confidence != conf {
		t.Errorf("call %s from %q: target=%q confidence=%q, want target=%q confidence=%q",
			e.Callee, e.Caller, e.Target, e.Confidence, target, conf)
	}
}

const pyCallsFixture = `import json as js
from pathlib import Path
from .util import helper as h


def top(x):
    return x


class Repo:
    """Docstring with a fake call: nope(x)
    def in_doc(self):
        pass
    """

    def load(self):
        self.parse()
        Repo.parse(self)
        top(1)
        h(2)
        print("fake(")  # commented(out)
        Path("p")
        js.dumps({})
        missing()
        return len(self)

    def parse(self):
        return self.missing_method()
`

func TestPythonCallsAreAttributedAndResolved(t *testing.T) {
	const name = "pkg/mod.py"
	g, err := Extract(name, []byte(pyCallsFixture))
	if err != nil {
		t.Fatal(err)
	}
	src := pyCallsFixture
	expectCall(t, callAt(t, g, src, "self.parse()"), "pkg/mod.py#Repo.parse", ConfResolved)
	expectCall(t, callAt(t, g, src, "Repo.parse(self)"), "pkg/mod.py#Repo.parse", ConfResolved)
	expectCall(t, callAt(t, g, src, "top(1)"), "pkg/mod.py#top", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "h(2)"), ".util.helper", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "Path(\"p\")"), "pathlib.Path", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "js.dumps"), "json.dumps", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "missing()"), "", ConfUnresolved)
	expectCall(t, callAt(t, g, src, "self.missing_method()"), "", ConfUnresolved)

	if e := callAt(t, g, src, "self.parse()"); e.Caller != "Repo.load" {
		t.Errorf("self.parse() caller = %q, want Repo.load", e.Caller)
	}
	if e := callAt(t, g, src, "top(1)"); e.Caller != "Repo.load" || e.Qual != "" {
		t.Errorf("top(1) = %+v", e)
	}
	if e := callAt(t, g, src, "js.dumps"); e.Qual != "js" || e.Callee != "dumps" {
		t.Errorf("js.dumps qualifier = %+v", e)
	}
}

// Negative cases: text that looks like a call or a definition but is not code.
func TestPythonIgnoresStringsCommentsAndBuiltins(t *testing.T) {
	g, err := Extract("pkg/mod.py", []byte(pyCallsFixture))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range g.Edges {
		if e.Kind != EdgeCall {
			continue
		}
		switch e.Callee {
		case "nope", "fake", "commented", "print", "len", "in_doc":
			t.Errorf("recorded a call that is not code: %+v", e)
		}
	}
	for _, s := range g.Symbols {
		if s.Name == "in_doc" {
			t.Errorf("a def inside a docstring became a symbol: %+v", s)
		}
	}
}

const jsCallsFixture = `import { Api as Client, type Opts } from './client';
import * as lib from 'lib';
import fallback from 'fallback';
const fs = require('fs');

function helper(n) {
  return n;
}

export class Service {
  run() {
    this.step();
    helper(1);
    Client(2);
    lib.make();
    fallback();
    fs.readFile('x');
    console.log("nope(");
    unknown();
  }

  step() {
    return 1;
  }
}
`

func TestScriptCallsAreAttributedAndResolved(t *testing.T) {
	const name = "src/app.ts"
	g, err := Extract(name, []byte(jsCallsFixture))
	if err != nil {
		t.Fatal(err)
	}
	src := jsCallsFixture
	expectCall(t, callAt(t, g, src, "this.step()"), "src/app.ts#Service.step", ConfResolved)
	expectCall(t, callAt(t, g, src, "helper(1)"), "src/app.ts#helper", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "Client(2)"), "./client.Api", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "lib.make()"), "lib.make", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "fallback()"), "fallback", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "fs.readFile"), "fs.readFile", ConfHeuristic)
	expectCall(t, callAt(t, g, src, "console.log"), "", ConfUnresolved)
	expectCall(t, callAt(t, g, src, "unknown()"), "", ConfUnresolved)

	if e := callAt(t, g, src, "this.step()"); e.Caller != "Service.run" {
		t.Errorf("this.step() caller = %q, want Service.run", e.Caller)
	}
	for _, e := range g.Edges {
		if e.Kind == EdgeCall && e.Callee == "nope" {
			t.Errorf("a call inside a string was recorded: %+v", e)
		}
	}
	// A method body's `forEach(` is a call, not a method declaration.
	for _, s := range g.Symbols {
		if s.Name == "readFile" || s.Name == "readFile(" {
			t.Errorf("a call was declared as a method: %+v", s)
		}
	}
}

func TestCodeOnlyBlanksLiteralsAndComments(t *testing.T) {
	var st litState
	got := codeOnly(`x = f("a(b") # g(c)`, &st, LangPython)
	if strings.Contains(got, "a(b") || strings.Contains(got, "g(c") || !strings.Contains(got, "f(") {
		t.Errorf("python line = %q", got)
	}
	if len(got) != len(`x = f("a(b") # g(c)`) {
		t.Errorf("columns not preserved: %q", got)
	}

	st = litState{}
	if codeOnly(`s = """open`, &st, LangPython); st.close != `"""` {
		t.Errorf("open triple string not carried: state %q", st.close)
	}
	if got := codeOnly(`still inside """ foo(x)`, &st, LangPython); st.close != "" || strings.TrimSpace(got) != "foo(x)" {
		t.Errorf("closing line: got %q state %q", got, st.close)
	}

	st = litState{}
	if got := codeOnly("a(); /* b(", &st, LangJavaScript); st.close != "*/" || !strings.HasPrefix(got, "a(); ") || strings.Contains(got, "b(") {
		t.Errorf("block comment: got %q state %q", got, st.close)
	}
}

// goAmbiguousRepo has the names the old name-matching extractor conflated:
// the same function name in two packages, the same method name on two types,
// a method promoted from an embedded struct, and an interface call.
func goAmbiguousRepo() map[string]string {
	return map[string]string{
		"a/a.go": "package a\n\nfunc Save() {}\n\nfunc Run() {\n\tSave()\n}\n",
		"b/b.go": "package b\n\nfunc Save() {}\n\nfunc Use() {\n\tSave()\n}\n\n" +
			"type A struct{}\n\nfunc (A) Save() {}\n\ntype B struct{}\n\nfunc (*B) Save() {}\n\n" +
			"func Both() {\n\tvar a A\n\ta.Save()\n\tvar b B\n\tb.Save()\n}\n",
		"m/m.go": "package m\n\nimport \"strings\"\n\ntype Base struct{}\n\nfunc (Base) Ping() {}\n\n" +
			"type Outer struct{ Base }\n\nfunc (o Outer) Go() {\n\to.Ping()\n}\n\n" +
			"type Saver interface{ Save() }\n\nfunc Notify(s Saver) {\n\ts.Save()\n}\n\n" +
			"func Upper() string {\n\treturn strings.ToUpper(\"x\")\n}\n",
	}
}

func findCaller(t *testing.T, list []Caller, caller string) Caller {
	t.Helper()
	for _, c := range list {
		if c.Caller == caller {
			return c
		}
	}
	t.Fatalf("no call from %q in %+v", caller, list)
	return Caller{}
}

func TestGoCallersResolveWhereNamesWereAmbiguous(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, goAmbiguousRepo())
	mustIndex(t, s, root)

	// The same function name in two packages: each call reaches its own.
	inA, _ := s.Callers("a#Save", 50)
	if len(inA) != 1 || inA[0].Path != "a/a.go" || inA[0].Caller != "Run" || inA[0].Confidence != ConfResolved {
		t.Errorf("callers of a#Save = %+v, want only Run in a/a.go", inA)
	}
	inB, _ := s.Callers("b#Save", 50)
	if len(inB) != 1 || inB[0].Path != "b/b.go" || inB[0].Caller != "Use" {
		t.Errorf("callers of b#Save = %+v, want only Use in b/b.go", inB)
	}

	// The same method name on two types: a.Save() is A.Save, b.Save() is B.Save.
	toA, _ := s.Callers("b#A.Save", 50)
	if len(toA) != 1 || toA[0].Caller != "Both" || toA[0].Line != lineOf(goAmbiguousRepo()["b/b.go"], "a.Save()") {
		t.Errorf("callers of b#A.Save = %+v", toA)
	}
	toB, _ := s.Callers("b#B.Save", 50)
	if len(toB) != 1 || toB[0].Caller != "Both" || toB[0].Line != lineOf(goAmbiguousRepo()["b/b.go"], "b.Save()") {
		t.Errorf("callers of b#B.Save = %+v", toB)
	}
	// The dotted form reaches the same resolved call through its target.
	if dotted, _ := s.Callers("A.Save", 50); len(dotted) != 1 || dotted[0].Target != "b#A.Save" {
		t.Errorf("callers of A.Save = %+v, want the one resolved to b#A.Save", dotted)
	}

	// Bare name stays the broad answer: every call to anything called Save.
	all, _ := s.Callers("Save", 50)
	if len(all) != 5 {
		t.Errorf("bare Save found %d calls, want 5: %+v", len(all), all)
	}

	// Promotion: o.Ping() reaches Base.Ping through the embedded struct.
	ping, _ := s.Callers("m#Base.Ping", 50)
	if len(ping) != 1 || ping[0].Caller != "Outer.Go" || ping[0].Confidence != ConfResolved {
		t.Errorf("callers of m#Base.Ping = %+v", ping)
	}

	// An interface call is heuristic: it names the interface method, not an
	// implementation, which is not known here.
	iface, _ := s.Callers("m#Saver.Save", 50)
	if len(iface) != 1 || iface[0].Confidence != ConfHeuristic {
		t.Errorf("callers of m#Saver.Save = %+v", iface)
	}

	// Cross-package: the external target is importpath.Name and unresolved.
	up := findCaller(t, mustCallers(t, s, "ToUpper"), "Upper")
	if up.Target != "strings.ToUpper" || up.Confidence != ConfUnresolved {
		t.Errorf("strings.ToUpper edge = %+v", up)
	}
}

func mustCallers(t *testing.T, s *Store, name string) []Caller {
	t.Helper()
	list, err := s.Callers(name, 50)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

// A call's resolution depends on its sibling files, so changing a sibling must
// re-resolve it even though the calling file itself is byte-identical.
func TestGoSiblingChangeReresolvesCalls(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, map[string]string{
		"p/save.go": "package p\n\nfunc Save() {}\n",
		"p/run.go":  "package p\n\nfunc Run() {\n\tSave()\n}\n",
	})
	mustIndex(t, s, root)
	if got := mustCallers(t, s, "p#Save"); len(got) != 1 {
		t.Fatalf("before: %+v", got)
	}

	if err := os.WriteFile(filepath.Join(root, "p", "save.go"), []byte("package p\n\nfunc Store() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := mustIndex(t, s, root)
	if st.Indexed != 2 || st.Unchanged != 0 {
		t.Errorf("stats = %+v, want both package files rewritten", st)
	}
	if got := mustCallers(t, s, "p#Save"); len(got) != 0 {
		t.Errorf("after the declaration moved, a#Save still resolves: %+v", got)
	}
	run := findCaller(t, mustCallers(t, s, "Save"), "Run")
	if run.Confidence != ConfUnresolved || run.Target != "" {
		t.Errorf("Save() after its declaration was removed = %+v", run)
	}
}

// Call edges written before they had a confidence are re-parsed once, even
// though their files are unchanged.
func TestStaleCallEdgesAreReparsedOnce(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, goAmbiguousRepo())
	first := mustIndex(t, s, root)
	if _, err := s.DB.Conn().Exec("UPDATE code_edges SET confidence = '' WHERE kind = 'call'"); err != nil {
		t.Fatal(err)
	}
	again := mustIndex(t, s, root)
	if again.Unchanged != 0 || again.Indexed != first.Indexed+first.Unchanged {
		t.Errorf("after upgrade: %+v (first run %+v)", again, first)
	}
	if got := mustCallers(t, s, "b#A.Save"); len(got) != 1 {
		t.Errorf("confidence not restored: %+v", got)
	}
	if stale, _ := s.staleCallEdges(root); stale {
		t.Error("edges still stale after the re-parse")
	}
}

// A file that does not parse must not stop its package from resolving.
func TestGoBrokenSiblingDoesNotBlockResolution(t *testing.T) {
	s := openStore(t)
	root := writeRepo(t, map[string]string{
		"q/ok.go":     "package q\n\nfunc Save() {}\n\nfunc Run() {\n\tSave()\n}\n",
		"q/broken.go": "package q\n\nfunc (\n",
	})
	st := mustIndex(t, s, root)
	if st.Failed != 1 {
		t.Errorf("failed = %d, want the broken file only", st.Failed)
	}
	if got := mustCallers(t, s, "q#Save"); len(got) != 1 {
		t.Errorf("Save in the good file did not resolve: %+v", got)
	}
}
