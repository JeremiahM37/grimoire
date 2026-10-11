package codegraph

import (
	"regexp"
	"strings"
)

// JavaScript and TypeScript are read with patterns, the same way Python is.
// Declarations are found line by line and their blocks are tracked by
// indentation, which is fine for the formatting real projects use and wrong
// for minified one-liners, which the size cap mostly keeps out. Call edges are
// attributed by brace depth instead, because a call belongs to the innermost
// open brace, not to the indentation of its line.
var (
	jsFunc    = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:async\s+)?function\s*\*?\s*([A-Za-z_$][\w$]*)`)
	jsClass   = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	jsIface   = regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	jsType    = regexp.MustCompile(`^\s*(?:export\s+)?(?:declare\s+)?type\s+([A-Za-z_$][\w$]*)\s*(?:<[^=]*>)?\s*=`)
	jsEnum    = regexp.MustCompile(`^\s*(?:export\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`)
	jsVar     = regexp.MustCompile(`^\s*(?:export\s+)?(const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(.*)$`)
	jsArrow   = regexp.MustCompile(`^(?:async\s+)?(?:function\b|\([^)]*\)\s*(?::[^=]*)?=>|[A-Za-z_$][\w$]*\s*=>)`)
	jsMethod  = regexp.MustCompile(`^\s*(?:(?:public|private|protected|static|async|readonly|override|abstract|get|set)\s+)*\*?\s*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\s*\(`)
	jsImport  = regexp.MustCompile(`^\s*import\s+(?:type\s+)?(?:[^'"]*?\s+from\s+)?['"]([^'"]+)['"]`)
	jsClause  = regexp.MustCompile(`^\s*import\s+(?:type\s+)?(.+?)\s+from\s+['"]([^'"]+)['"]`)
	jsExport  = regexp.MustCompile(`^\s*export\s+(?:type\s+)?[^'"]*?\bfrom\s+['"]([^'"]+)['"]`)
	jsRequire = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsReqBind = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsCloser  = regexp.MustCompile(`^\s*\}\s*from\s+['"]([^'"]+)['"]`)
	jsCall    = regexp.MustCompile(`([A-Za-z_$][\w$.]*)\s*\(`)
)

// jsKeywords are words that look like a method call inside a class body
// (`if (x) {`) but are not declarations.
var jsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "with": true, "do": true,
}

// jsNotCalls are words that read as calls (`typeof (x)`) and are not edges.
var jsNotCalls = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "with": true, "typeof": true, "await": true,
	"void": true, "delete": true, "super": true, "require": true, "import": true,
	"new": true, "else": true, "throw": true, "yield": true, "in": true, "of": true,
	"case": true, "do": true,
}

// braceFrame is one open block that declares a class or a function. depth is
// the brace depth outside the block, so the block is closed when the depth
// falls back to it.
type braceFrame struct {
	depth int
	kind  string
	qual  string // for a function, the name a call inside it is attributed to
	cls   string // for a method, the class it belongs to
}

// jsCallSite is one call before resolution.
type jsCallSite struct {
	line   int
	caller string
	cls    string
	callee string
	qual   string
}

// jsBinding is what an imported local name stands for. bare is the target of a
// call to the name itself; ns is the module a member call on it reaches.
type jsBinding struct{ bare, ns string }

// extractScript reads JavaScript and TypeScript declarations, imports and call
// edges. name is the file, used for the target of a call into this file.
func extractScript(name, lang string, src []byte) FileGraph {
	lines := splitLines(src)
	g := FileGraph{Lang: lang}
	var stack []scope
	var frames []braceFrame
	var sites []jsCallSite
	bindings := map[string]jsBinding{}
	depth := 0
	var lit litState
	add := func(sym string, kind string, line, ind int, braces bool) {
		g.Symbols = append(g.Symbols, Symbol{Name: sym, Kind: kind, Scope: enclosingClass(stack),
			Line: line, EndLine: blockEnd(lines, line-1, ind, braces)})
	}

	for i, raw := range lines {
		inString := lit.close != ""
		code := codeOnly(raw, &lit, lang)
		line := i + 1
		segment := ""
		if !inString && strings.TrimSpace(code) != "" {
			ind := indentOf(raw)
			stack = popTo(stack, ind)
			segment = declare(raw, code, line, ind, depth, &frames, &stack, &g, add, bindings)
		}
		if segment != "" {
			sites = append(sites, jsCallSites(line, segment, frames)...)
		}
		depth += strings.Count(code, "{") - strings.Count(code, "}")
		for len(frames) > 0 && depth <= frames[len(frames)-1].depth {
			frames = frames[:len(frames)-1]
		}
	}

	r := newJSResolver(name, g.Symbols, bindings)
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

// declare handles one line that is not inside a literal. It records any
// declaration, import or export, and returns the code on this line that runs
// inside the current block and so may hold calls. A declaration's own header is
// not code to scan, so for a declaration the scan starts after its open brace.
func declare(raw, code string, line, ind, depth int, frames *[]braceFrame, stack *[]scope,
	g *FileGraph, add func(string, string, int, int, bool), bindings map[string]jsBinding) string {
	t := strings.TrimSpace(code)
	open := func(sym, kind, qual string) string {
		*frames = append(*frames, braceFrame{depth: depth, kind: KindFunc, qual: qual})
		*stack = append(*stack, scope{indent: ind, name: sym, kind: kind})
		return afterBrace(code)
	}

	if m := jsImport.FindStringSubmatch(raw); m != nil {
		g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
		if c := jsClause.FindStringSubmatch(raw); c != nil {
			bindImport(c[1], c[2], bindings)
		}
		return ""
	}
	if m := jsExport.FindStringSubmatch(raw); m != nil {
		g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
		return ""
	}
	if m := jsCloser.FindStringSubmatch(raw); m != nil {
		g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
		return ""
	}
	if m := jsReqBind.FindStringSubmatch(raw); m != nil {
		g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[2]})
		bindings[m[1]] = jsBinding{bare: m[2], ns: m[2]}
		return ""
	}
	if m := jsRequire.FindStringSubmatch(raw); m != nil && strings.Contains(raw, "= require(") {
		g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
		return ""
	}
	if m := jsClass.FindStringSubmatch(code); m != nil {
		add(m[1], KindClass, line, ind, true)
		*stack = append(*stack, scope{indent: ind, name: m[1], kind: KindClass})
		*frames = append(*frames, braceFrame{depth: depth, kind: KindClass, qual: m[1], cls: m[1]})
		return afterBrace(code)
	}
	if m := jsIface.FindStringSubmatch(code); m != nil {
		add(m[1], KindInterface, line, ind, true)
		return ""
	}
	if m := jsEnum.FindStringSubmatch(code); m != nil {
		add(m[1], KindEnum, line, ind, true)
		return ""
	}
	if m := jsType.FindStringSubmatch(code); m != nil {
		add(m[1], KindType, line, ind, false)
		return ""
	}
	if m := jsFunc.FindStringSubmatch(code); m != nil {
		add(m[1], KindFunc, line, ind, true)
		return open(m[1], KindFunc, m[1])
	}
	if m := jsVar.FindStringSubmatch(code); m != nil && len(*stack) == 0 {
		kind := KindVar
		if m[1] == "const" {
			kind = KindConst
		}
		if jsArrow.MatchString(strings.TrimSpace(m[3])) {
			kind = KindFunc
		}
		add(m[2], kind, line, ind, true)
		// A function body, or an object literal, opens a block that holds its
		// own local declarations; those are not top-level.
		if kind == KindFunc {
			return open(m[2], kind, m[2])
		}
		if strings.HasSuffix(t, "{") {
			*stack = append(*stack, scope{indent: ind, name: m[2], kind: kind})
		}
		return code
	}
	if cls := enclosingClass(*stack); cls != "" && strings.HasSuffix(t, "{") && len(*frames) > 0 &&
		(*frames)[len(*frames)-1].kind == KindClass {
		if m := jsMethod.FindStringSubmatch(code); m != nil && !jsKeywords[m[1]] {
			add(m[1], KindMethod, line, ind, true)
			*frames = append(*frames, braceFrame{depth: depth, kind: KindFunc,
				qual: cls + "." + m[1], cls: cls})
			return afterBrace(code)
		}
	}
	return code
}

// afterBrace is the code after a declaration's open brace, or "" when the
// declaration does not open a block on this line.
func afterBrace(code string) string {
	i := strings.Index(code, "{")
	if i < 0 {
		return ""
	}
	return code[i+1:]
}

// bindImport reads the clause of an import (`a, { b as c }`, `* as ns`, or a
// default name) and records what each local name stands for.
func bindImport(clause, mod string, bindings map[string]jsBinding) {
	if open := strings.Index(clause, "{"); open >= 0 {
		close := strings.LastIndex(clause, "}")
		if close > open {
			for _, part := range strings.Split(clause[open+1:close], ",") {
				part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "type "))
				if part == "" {
					continue
				}
				orig, local, _ := strings.Cut(part, " as ")
				orig, local = strings.TrimSpace(orig), strings.TrimSpace(local)
				if local == "" {
					local = orig
				}
				bindings[local] = jsBinding{bare: mod + "." + orig}
			}
		}
	}
	if i := strings.Index(clause, "* as "); i >= 0 {
		ns := strings.TrimSpace(clause[i+len("* as "):])
		bindings[ns] = jsBinding{ns: mod}
	}
	head := clause
	if i := strings.Index(head, "{"); i >= 0 {
		head = head[:i]
	}
	head = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(head), ","))
	if head != "" && !strings.Contains(head, "*") {
		bindings[head] = jsBinding{bare: mod, ns: mod}
	}
}

// jsCallSites finds the calls in a segment of code that runs inside the
// innermost open function.
func jsCallSites(line int, segment string, frames []braceFrame) []jsCallSite {
	caller, cls := "", ""
	for i := len(frames) - 1; i >= 0; i-- {
		if frames[i].kind == KindFunc {
			caller, cls = frames[i].qual, frames[i].cls
			break
		}
	}
	var out []jsCallSite
	for _, m := range jsCall.FindAllStringSubmatch(segment, -1) {
		chain := m[1]
		qual, callee := "", chain
		if i := strings.LastIndex(chain, "."); i >= 0 {
			qual, callee = chain[:i], chain[i+1:]
		}
		if callee == "" || jsNotCalls[callee] && qual == "" {
			continue
		}
		out = append(out, jsCallSite{line: line, caller: caller, cls: cls, callee: callee, qual: qual})
	}
	return out
}

// jsResolver resolves the call sites of one file against its declarations and
// imports, in the same way as the Python resolver.
type jsResolver struct {
	file     string
	methods  map[string]map[string]bool
	funcs    map[string]bool
	classes  map[string]bool
	bindings map[string]jsBinding
}

func newJSResolver(file string, syms []Symbol, bindings map[string]jsBinding) *jsResolver {
	r := &jsResolver{file: file, methods: map[string]map[string]bool{},
		funcs: map[string]bool{}, classes: map[string]bool{}, bindings: bindings}
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
// that is not an edge at all.
func (r *jsResolver) resolve(s jsCallSite) (target, conf string, ok bool) {
	local := func(decl string) string { return r.file + "#" + decl }
	if s.qual == "this" {
		if s.cls != "" && r.methods[s.cls][s.callee] {
			return local(s.cls + "." + s.callee), ConfResolved, true
		}
		return "", ConfUnresolved, true
	}
	if s.qual == "" {
		if r.funcs[s.callee] || r.classes[s.callee] {
			return local(s.callee), ConfHeuristic, true
		}
		if b := r.bindings[s.callee]; b.bare != "" {
			return b.bare, ConfHeuristic, true
		}
		return "", ConfUnresolved, true
	}
	if r.classes[s.qual] && r.methods[s.qual][s.callee] {
		return local(s.qual + "." + s.callee), ConfResolved, true
	}
	first, rest, _ := strings.Cut(s.qual, ".")
	if b, found := r.bindings[first]; found && b.ns != "" {
		t := b.ns
		if rest != "" {
			t += "." + rest
		}
		return t + "." + s.callee, ConfHeuristic, true
	}
	return "", ConfUnresolved, true
}
