package codegraph

import (
	"regexp"
	"strings"
)

var (
	pyDef    = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pyClass  = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyImport = regexp.MustCompile(`^\s*import\s+(.+)$`)
	pyFrom   = regexp.MustCompile(`^\s*from\s+([.\w]+)\s+import\s+(.+)$`)
	// pyCall matches a dotted name directly followed by an open parenthesis.
	// The whole chain is captured, so a.b.c() is one match with qualifier a.b.
	pyCall = regexp.MustCompile(`([A-Za-z_][\w.]*)\s*\(`)
)

// pyKeywords are words that read as calls (`if (x):`) but are not.
var pyKeywords = map[string]bool{
	"if": true, "elif": true, "while": true, "for": true, "with": true,
	"return": true, "yield": true, "await": true, "not": true, "and": true,
	"or": true, "in": true, "is": true, "lambda": true, "except": true,
	"assert": true, "del": true, "raise": true, "def": true, "class": true,
	"async": true, "import": true, "from": true,
}

// pyBuiltins are the predeclared functions. A bare call to one of these, with no
// definition or import of the same name in the file, is not recorded.
var pyBuiltins = map[string]bool{
	"print": true, "len": true, "range": true, "isinstance": true, "issubclass": true,
	"super": true, "str": true, "int": true, "float": true, "bool": true,
	"list": true, "dict": true, "set": true, "tuple": true, "frozenset": true,
	"bytes": true, "open": true, "enumerate": true, "zip": true, "map": true,
	"filter": true, "sorted": true, "reversed": true, "min": true, "max": true,
	"sum": true, "any": true, "all": true, "abs": true, "round": true,
	"getattr": true, "setattr": true, "hasattr": true, "delattr": true,
	"repr": true, "iter": true, "next": true, "type": true, "object": true,
	"id": true, "hash": true, "format": true, "vars": true, "dir": true,
	"callable": true, "divmod": true, "pow": true, "input": true,
	"property": true, "staticmethod": true, "classmethod": true, "exit": true,
}

// pyCallSite is one call found in a Python file before resolution.
type pyCallSite struct {
	line   int
	caller string // qualified name of the enclosing function, "" at module level
	cls    string // the class the enclosing method belongs to, "" otherwise
	callee string
	qual   string // the full dotted qualifier, "" for a bare call
}

// extractPython reads Python declarations, imports and call edges line by line.
// Nesting is tracked by indentation, which is how Python itself delimits blocks,
// so a def under a class is a method of that class. Strings and comments are
// blanked before scanning, and so are the bodies of triple-quoted strings, which
// is what stops a docstring that shows an example call from becoming an edge.
//
// Calls are attributed to the innermost enclosing def. They are resolved after
// the file is read, because a call may name a method defined further down: a
// self.m() inside a class is resolved when the class defines m; a bare name is
// heuristic when this file defines it or imports it; anything else is
// unresolved, with the name kept so a name query still finds it.
func extractPython(name string, src []byte) FileGraph {
	lines := splitLines(src)
	g := FileGraph{Lang: LangPython}
	var stack []scope
	var sites []pyCallSite
	imports := map[string]string{} // local name -> what it stands for
	var lit litState
	for i, raw := range lines {
		inString := lit.close != ""
		code := codeOnly(raw, &lit, LangPython)
		if inString {
			continue
		}
		t := strings.TrimSpace(code)
		if t == "" {
			continue
		}
		ind := indentOf(raw)
		stack = popTo(stack, ind)

		if m := pyImport.FindStringSubmatch(code); m != nil {
			for _, part := range strings.Split(m[1], ",") {
				part = strings.TrimSpace(part)
				if part == "" {
					continue
				}
				e := Edge{Kind: EdgeImport, Line: i + 1, Callee: part}
				if mod, alias, ok := strings.Cut(part, " as "); ok {
					e.Callee, e.Qual = strings.TrimSpace(mod), strings.TrimSpace(alias)
					imports[e.Qual] = e.Callee
				} else {
					// `import a.b` binds the top name a, and a.b.f() starts from it.
					top := strings.SplitN(part, ".", 2)[0]
					imports[top] = top
				}
				g.Edges = append(g.Edges, e)
			}
			continue
		}
		if m := pyFrom.FindStringSubmatch(code); m != nil {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: i + 1, Callee: m[1]})
			for _, part := range strings.Split(strings.Trim(m[2], "() \t"), ",") {
				part = strings.TrimSpace(part)
				if part == "" || part == "*" {
					continue
				}
				orig, local, _ := strings.Cut(part, " as ")
				orig = strings.TrimSpace(orig)
				local = strings.TrimSpace(local)
				if local == "" {
					local = orig
				}
				imports[local] = m[1] + "." + orig
			}
			continue
		}
		if m := pyClass.FindStringSubmatch(code); m != nil {
			scopeName := enclosingClass(stack)
			g.Symbols = append(g.Symbols, Symbol{Name: m[1], Kind: KindClass, Scope: scopeName,
				Line: i + 1, EndLine: blockEnd(lines, i, ind, false)})
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindClass, qual: m[1]})
			continue
		}
		if m := pyDef.FindStringSubmatch(code); m != nil {
			kind, owner := KindFunc, ""
			if cls := enclosingClass(stack); cls != "" {
				kind, owner = KindMethod, cls
			}
			sym := Symbol{Name: m[1], Kind: kind, Scope: owner,
				Line: i + 1, EndLine: blockEnd(lines, i, ind, false)}
			g.Symbols = append(g.Symbols, sym)
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindFunc, qual: sym.Qualified()})
			continue
		}
		sites = append(sites, pyCallSites(i+1, code, stack)...)
	}

	r := newPyResolver(name, g.Symbols, imports)
	for _, s := range sites {
		target, conf, ok := r.resolve(s)
		if !ok {
			continue
		}
		g.Edges = append(g.Edges, Edge{Kind: EdgeCall, Line: s.line, Caller: s.caller,
			Callee: s.callee, Qual: s.qual, Target: target, Confidence: conf})
	}
	return g
}

// pyCallSites finds the calls on one line of code, attributed to the innermost
// enclosing def.
func pyCallSites(line int, code string, stack []scope) []pyCallSite {
	caller, cls := pyEnclosing(stack)
	var out []pyCallSite
	for _, m := range pyCall.FindAllStringSubmatch(code, -1) {
		chain := m[1]
		qual, callee := "", chain
		if i := strings.LastIndex(chain, "."); i >= 0 {
			qual, callee = chain[:i], chain[i+1:]
		}
		if callee == "" || pyKeywords[callee] && qual == "" {
			continue
		}
		out = append(out, pyCallSite{line: line, caller: caller, cls: cls,
			callee: callee, qual: qual})
	}
	return out
}

// pyEnclosing is the qualified name of the innermost def, and the class that
// def is a method of. Both are "" at module level or directly in a class body.
func pyEnclosing(stack []scope) (caller, cls string) {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].kind != KindFunc {
			continue
		}
		if i > 0 && stack[i-1].kind == KindClass {
			cls = stack[i-1].name
		}
		return stack[i].qual, cls
	}
	return "", ""
}

// pyResolver resolves the call sites of one file against its own declarations
// and imports.
type pyResolver struct {
	file    string
	methods map[string]map[string]bool // class -> its methods
	funcs   map[string]bool            // module-level functions
	classes map[string]bool            // module-level classes
	imports map[string]string
}

func newPyResolver(file string, syms []Symbol, imports map[string]string) *pyResolver {
	r := &pyResolver{file: file, methods: map[string]map[string]bool{},
		funcs: map[string]bool{}, classes: map[string]bool{}, imports: imports}
	for _, s := range syms {
		switch {
		case s.Kind == KindMethod:
			if r.methods[s.Scope] == nil {
				r.methods[s.Scope] = map[string]bool{}
			}
			r.methods[s.Scope][s.Name] = true
		case s.Kind == KindFunc && s.Scope == "":
			r.funcs[s.Name] = true
		case s.Kind == KindClass && s.Scope == "":
			r.classes[s.Name] = true
		}
	}
	return r
}

// resolve gives the target and confidence of one call. ok is false for a call
// that is not an edge at all (a builtin with no local definition).
func (r *pyResolver) resolve(s pyCallSite) (target, conf string, ok bool) {
	local := func(decl string) string { return r.file + "#" + decl }
	switch {
	case s.qual == "self" || s.qual == "cls":
		if s.cls != "" && r.methods[s.cls][s.callee] {
			return local(s.cls + "." + s.callee), ConfResolved, true
		}
		return "", ConfUnresolved, true

	case s.qual == "":
		if r.funcs[s.callee] {
			return local(s.callee), ConfHeuristic, true
		}
		if r.classes[s.callee] {
			return local(s.callee), ConfHeuristic, true
		}
		if imp, found := r.imports[s.callee]; found {
			return imp, ConfHeuristic, true
		}
		if pyBuiltins[s.callee] {
			return "", "", false
		}
		return "", ConfUnresolved, true
	}

	// A qualified call. A class defined here, called by name, is resolved like
	// self when the class has the method.
	if r.classes[s.qual] && r.methods[s.qual][s.callee] {
		return local(s.qual + "." + s.callee), ConfResolved, true
	}
	first, rest, _ := strings.Cut(s.qual, ".")
	if root, found := r.imports[first]; found {
		target := root
		if rest != "" {
			target += "." + rest
		}
		return target + "." + s.callee, ConfHeuristic, true
	}
	return "", ConfUnresolved, true
}

// enclosingClass is the innermost open block when it is a class, else "".
func enclosingClass(stack []scope) string {
	if len(stack) == 0 || stack[len(stack)-1].kind != KindClass {
		return ""
	}
	return stack[len(stack)-1].name
}
