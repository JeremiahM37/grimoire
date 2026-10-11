package codegraph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"sort"
	"strconv"
)

// builtins are the predeclared functions. A bare call to one of these is not
// an edge to anything in the repository, so it is dropped rather than letting
// "len" show up as a caller of every package. The type checker is asked first
// when it ran: a package that declares its own len is not the builtin.
var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// goSource is one Go file of a package directory, as the extractor receives it.
type goSource struct {
	rel string // slash-separated, relative to the repository root
	src []byte
}

// goResult is one file's outcome: its graph, or the parse error that keeps it
// out of the index.
type goResult struct {
	g   FileGraph
	err error
}

// extractGo reads one Go file on its own. Its package is that file, so calls
// resolve only to declarations in the same file; Index extracts a whole
// directory at once, which is what resolves calls across files.
func extractGo(name string, src []byte) (FileGraph, error) {
	dir := path.Dir(name)
	r := extractGoDir(dir, []goSource{{rel: name, src: src}})[name]
	return r.g, r.err
}

// extractGoDir parses the Go files of one directory and extracts each of them.
// Files are grouped by package clause (a directory may hold `x` and `x_test`),
// and each group is type-checked as one package, so a call in one file reaches
// the declaration in a sibling file. A file that does not parse is an error for
// that file only; the rest of the directory is still resolved.
func extractGoDir(dir string, files []goSource) map[string]goResult {
	out := make(map[string]goResult, len(files))
	fset := token.NewFileSet()
	groups := map[string][]goFile{}
	var order []string
	for _, src := range files {
		f, err := parser.ParseFile(fset, src.rel, src.src, parser.SkipObjectResolution)
		if err != nil {
			out[src.rel] = goResult{err: fmt.Errorf("parsing %s: %w", src.rel, err)}
			continue
		}
		name := f.Name.Name
		if _, seen := groups[name]; !seen {
			order = append(order, name)
		}
		groups[name] = append(groups[name], goFile{rel: src.rel, ast: f})
	}
	sort.Strings(order)
	for _, name := range order {
		for rel, fg := range goPackageGraphs(fset, dir, name, groups[name]) {
			out[rel] = goResult{g: fg}
		}
	}
	return out
}

// goFile is one parsed file in a package group.
type goFile struct {
	rel string
	ast *ast.File
}

// goDeclarations reads the symbols and imports of one file. Calls are added by
// goPackageGraphs, because resolving them needs the whole package.
func goDeclarations(fset *token.FileSet, f *ast.File) FileGraph {
	g := FileGraph{Lang: LangGo}
	line := func(p token.Pos) int { return fset.Position(p).Line }

	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		e := Edge{Kind: EdgeImport, Line: line(imp.Pos()), Callee: p}
		if imp.Name != nil {
			e.Qual = imp.Name.Name
		}
		g.Edges = append(g.Edges, e)
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			sym := Symbol{
				Name:    d.Name.Name,
				Kind:    KindFunc,
				Line:    line(d.Pos()),
				EndLine: line(d.End()),
			}
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sym.Kind = KindMethod
				sym.Scope = receiverType(d.Recv.List[0].Type)
			}
			g.Symbols = append(g.Symbols, sym)
		case *ast.GenDecl:
			g.Symbols = append(g.Symbols, genSymbols(fset, d)...)
		}
	}
	return g
}

// goPackageGraphs extracts every file of one package. When the package is too
// large to type-check in time, its calls are still recorded, by name, marked
// unresolved; see gotypes.go for the bounds.
func goPackageGraphs(fset *token.FileSet, dir, pkgName string, files []goFile) map[string]FileGraph {
	rs := checkGoPackage(fset, dir, pkgName, files)
	out := make(map[string]FileGraph, len(files))
	for _, f := range files {
		g := goDeclarations(fset, f.ast)
		for _, decl := range f.ast.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Body != nil {
					g.Edges = append(g.Edges, callsIn(fset, d.Body, goSymbolName(d), rs)...)
				}
			case *ast.GenDecl:
				// Package-level var initialisers can call functions too; they
				// have no enclosing function, so their caller is empty.
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, v := range vs.Values {
							g.Edges = append(g.Edges, callsIn(fset, v, "", rs)...)
						}
					}
				}
			}
		}
		out[f.rel] = g
	}
	return out
}

// goSymbolName is the qualified name the symbol table uses for a function, so
// that a call's Caller matches the Symbol it sits in.
func goSymbolName(d *ast.FuncDecl) string {
	sym := Symbol{Name: d.Name.Name, Kind: KindFunc}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sym.Scope = receiverType(d.Recv.List[0].Type)
	}
	return sym.Qualified()
}

func genSymbols(fset *token.FileSet, d *ast.GenDecl) []Symbol {
	var out []Symbol
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			kind := KindType
			switch s.Type.(type) {
			case *ast.StructType:
				kind = KindStruct
			case *ast.InterfaceType:
				kind = KindInterface
			}
			out = append(out, Symbol{Name: s.Name.Name, Kind: kind,
				Line: fset.Position(s.Pos()).Line, EndLine: fset.Position(s.End()).Line})
		case *ast.ValueSpec:
			kind := KindVar
			if d.Tok == token.CONST {
				kind = KindConst
			}
			for _, id := range s.Names {
				if id.Name == "_" {
					continue
				}
				out = append(out, Symbol{Name: id.Name, Kind: kind,
					Line: fset.Position(id.Pos()).Line, EndLine: fset.Position(s.End()).Line})
			}
		}
	}
	return out
}

// receiverType returns the bare type name of a method receiver: *T, T[P] and
// *T[P] all give "T".
func receiverType(e ast.Expr) string {
	for {
		switch t := e.(type) {
		case *ast.StarExpr:
			e = t.X
		case *ast.ParenExpr:
			e = t.X
		case *ast.IndexExpr:
			e = t.X
		case *ast.IndexListExpr:
			e = t.X
		case *ast.Ident:
			return t.Name
		default:
			return ""
		}
	}
}

// callsIn collects every call inside node, attributed to caller. rs may be nil
// or have no type information, in which case each call is recorded by name and
// marked unresolved, which is what the extractor did before it had types.
func callsIn(fset *token.FileSet, node ast.Node, caller string, rs *goResolver) []Edge {
	var out []Edge
	ast.Inspect(node, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := call.Fun
		// Generic instantiation: f[int](x) and g[K, V](x).
		switch ix := fun.(type) {
		case *ast.IndexExpr:
			fun = ix.X
		case *ast.IndexListExpr:
			fun = ix.X
		}
		e := Edge{Kind: EdgeCall, Line: fset.Position(call.Pos()).Line, Caller: caller}
		if !rs.classify(fun, &e) {
			return true
		}
		out = append(out, e)
		return true
	})
	return out
}
