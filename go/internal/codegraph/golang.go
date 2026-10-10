package codegraph

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
)

// builtins are the predeclared functions. A bare call to one of these is not
// an edge to anything in the repository, so it is dropped rather than letting
// "len" show up as a caller of every package.
var builtins = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
}

// extractGo parses a Go file with go/parser. A file that does not parse is an
// error, not an empty graph: the caller records it, so a broken file is
// visible rather than silently absent from every answer.
func extractGo(name string, src []byte) (FileGraph, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return FileGraph{}, fmt.Errorf("parsing %s: %w", name, err)
	}
	g := FileGraph{Lang: LangGo}
	line := func(p token.Pos) int { return fset.Position(p).Line }

	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		e := Edge{Kind: EdgeImport, Line: line(imp.Pos()), Callee: path}
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
			if d.Body != nil {
				g.Edges = append(g.Edges, callsIn(fset, d.Body, sym.Qualified())...)
			}
		case *ast.GenDecl:
			g.Symbols = append(g.Symbols, genSymbols(fset, d)...)
			// Package-level var initialisers can call functions too; they have
			// no enclosing function, so their caller is empty.
			for _, spec := range d.Specs {
				if vs, ok := spec.(*ast.ValueSpec); ok {
					for _, v := range vs.Values {
						g.Edges = append(g.Edges, callsIn(fset, v, "")...)
					}
				}
			}
		}
	}
	return g, nil
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

// callsIn collects every call inside node, attributed to caller.
func callsIn(fset *token.FileSet, node ast.Node, caller string) []Edge {
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
		switch fn := fun.(type) {
		case *ast.Ident:
			if builtins[fn.Name] {
				return true
			}
			e.Callee = fn.Name
		case *ast.SelectorExpr:
			e.Callee = fn.Sel.Name
			// Only a plain identifier is recorded as the qualifier. A chain
			// like a.b.C() has no single name to match on, so it stays empty.
			if id, ok := fn.X.(*ast.Ident); ok {
				e.Qual = id.Name
			}
		default:
			return true
		}
		out = append(out, e)
		return true
	})
	return out
}
