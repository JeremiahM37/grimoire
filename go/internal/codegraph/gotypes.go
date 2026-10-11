package codegraph

import (
	"go/ast"
	"go/token"
	"go/types"
	"path"
	"strings"
	"time"
)

// The Go resolver runs go/types over one package directory, tolerantly.
//
// Nothing is compiled and no go tool runs. Imports are answered by a stub that
// returns an empty package, so every name that comes from outside the directory
// is undefined to the checker, and the checker goes on past the errors that
// causes (the errors are discarded). What remains is exact for everything
// declared in the package itself: a call to a package function, a method on a
// value whose type the checker could infer, and a method promoted from an
// embedded struct of this package all resolve to the declaration they reach.
// A call through an interface resolves to the interface method, which is a
// heuristic, since the concrete implementation is not known here.
//
// Bounds: a package over maxTypedPackageFiles files, or one whose check runs
// past maxTypeCheckTime, is not type-checked. Its calls are recorded by name
// and marked unresolved. A timed-out check keeps running in the background
// until it finishes, which is bounded by the file caps.
const (
	maxTypedPackageFiles = 400
	maxTypeCheckTime     = 10 * time.Second
)

// goResolver classifies the call sites of one package. A nil resolver, or one
// whose info is nil, classifies by name only.
type goResolver struct {
	pkg  *types.Package
	info *types.Info
	// dir is the package directory relative to the root; "." becomes "".
	dir string
}

// checkGoPackage type-checks one package group. It returns a resolver that
// always works: with type information when the check finished in time, and
// name-only otherwise.
func checkGoPackage(fset *token.FileSet, dir, pkgName string, files []goFile) *goResolver {
	key := dir
	if key == "." {
		key = ""
	}
	rs := &goResolver{dir: key}
	if len(files) > maxTypedPackageFiles {
		return rs
	}
	asts := make([]*ast.File, len(files))
	for i, f := range files {
		asts[i] = f.ast
	}
	info := &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}
	conf := types.Config{
		Importer:    stubImporter{pkgs: map[string]*types.Package{}},
		Error:       func(error) {}, // tolerant: keep going past errors
		FakeImportC: true,
	}
	type checked struct{ pkg *types.Package }
	done := make(chan checked, 1)
	go func() {
		// Check returns the package it built; objects are compared against
		// that one, not against any package made beforehand.
		pkg, _ := conf.Check(dir, fset, asts, info) // errors go to conf.Error
		done <- checked{pkg}
	}()
	select {
	case c := <-done:
		rs.pkg = c.pkg
		rs.info = info
	case <-time.After(maxTypeCheckTime):
		// Leave info nil: the check goroutine writes only to its own info.
	}
	return rs
}

// stubImporter answers every import with an empty, complete package. The
// checker then reports the outside names as undefined, which the resolver does
// not need: a package-qualified call is recognised from the PkgName it binds.
type stubImporter struct{ pkgs map[string]*types.Package }

func (s stubImporter) Import(p string) (*types.Package, error) {
	if pkg, ok := s.pkgs[p]; ok {
		return pkg, nil
	}
	pkg := types.NewPackage(p, guessPkgName(p))
	pkg.MarkComplete()
	s.pkgs[p] = pkg
	return pkg, nil
}

// guessPkgName is the package name an import path most likely declares. It is
// only used to bind the local name, and an alias in the import always wins, so
// a wrong guess costs a missed resolution, not a wrong one.
func guessPkgName(p string) string {
	base := path.Base(p)
	if strings.HasPrefix(base, "v") && len(base) > 1 && strings.Trim(base[1:], "0123456789") == "" {
		base = path.Base(path.Dir(p))
	}
	if i := strings.Index(base, "."); i >= 0 {
		base = base[:i]
	}
	base = strings.TrimPrefix(strings.TrimSuffix(base, "-go"), "go-")
	base = strings.ReplaceAll(base, "-", "_")
	if base == "" {
		return "x"
	}
	return base
}

// classify fills the callee fields of e from the call's function expression and
// reports whether the call is an edge at all. Builtins are not edges.
func (r *goResolver) classify(fun ast.Expr, e *Edge) bool {
	var info *types.Info
	if r != nil {
		info = r.info
	}
	e.Confidence = ConfUnresolved
	switch fn := fun.(type) {
	case *ast.Ident:
		e.Callee = fn.Name
		var obj types.Object
		if info != nil {
			obj = info.Uses[fn]
		}
		if _, ok := obj.(*types.Builtin); ok || (obj == nil && builtins[fn.Name]) {
			return false
		}
		if f, ok := obj.(*types.Func); ok && !isMethod(f) && f.Pkg() != nil {
			if f.Pkg() == r.pkg {
				e.Target, e.Confidence = r.key()+"#"+f.Name(), ConfResolved
			} else {
				e.Target = f.Pkg().Path() + "." + f.Name()
			}
		}
		return true

	case *ast.SelectorExpr:
		e.Callee = fn.Sel.Name
		if id, ok := fn.X.(*ast.Ident); ok {
			e.Qual = id.Name
		}
		if info == nil {
			return true
		}
		if sel, ok := info.Selections[fn]; ok {
			f, ok := sel.Obj().(*types.Func)
			if !ok || sel.Kind() == types.FieldVal || f.Pkg() == nil {
				return true
			}
			recv, iface := recvName(f)
			if f.Pkg() == r.pkg && recv != "" {
				e.Target = r.key() + "#" + recv + "." + f.Name()
				e.Confidence = ConfResolved
				if iface {
					e.Confidence = ConfHeuristic
				}
			} else {
				e.Target = externalName(f.Pkg().Path(), recv, f.Name())
			}
			return true
		}
		if id, ok := fn.X.(*ast.Ident); ok {
			if pn, ok := info.Uses[id].(*types.PkgName); ok {
				e.Target = externalName(pn.Imported().Path(), "", fn.Sel.Name)
			}
		}
		return true
	}
	return false
}

// key is the package directory as Target spells it.
func (r *goResolver) key() string {
	if r == nil {
		return ""
	}
	return r.dir
}

func isMethod(f *types.Func) bool {
	sig, ok := f.Type().(*types.Signature)
	return ok && sig.Recv() != nil
}

// recvName is the named receiver type of a method, and whether that type is an
// interface. A method value with no named receiver gives "".
func recvName(f *types.Func) (string, bool) {
	sig, ok := f.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return "", false
	}
	t := sig.Recv().Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok {
		return "", false
	}
	_, iface := n.Underlying().(*types.Interface)
	return n.Obj().Name(), iface
}

// externalName is the importpath.Name form of a target outside the repository.
// A method is importpath.Type.Name.
func externalName(importPath, recv, name string) string {
	if recv != "" {
		return importPath + "." + recv + "." + name
	}
	return importPath + "." + name
}
