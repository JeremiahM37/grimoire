package codegraph

import (
	"regexp"
	"strings"
)

var (
	pyDef    = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(`)
	pyClass  = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyImport = regexp.MustCompile(`^\s*import\s+(.+)$`)
	pyFrom   = regexp.MustCompile(`^\s*from\s+([.\w]+)\s+import\b`)
)

// extractPython reads Python declarations and imports line by line. Nesting is
// tracked by indentation, which is how Python itself delimits blocks, so a
// def under a class is a method of that class. There are no call edges: that
// needs a parser, and a regex over Python would mostly report noise.
func extractPython(src []byte) FileGraph {
	lines := splitLines(src)
	g := FileGraph{Lang: LangPython}
	var stack []scope
	for i, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		ind := indentOf(raw)
		stack = popTo(stack, ind)

		if m := pyImport.FindStringSubmatch(raw); m != nil {
			for _, part := range strings.Split(m[1], ",") {
				part = strings.TrimSpace(strings.SplitN(part, "#", 2)[0])
				if part == "" {
					continue
				}
				e := Edge{Kind: EdgeImport, Line: i + 1, Callee: part}
				if mod, alias, ok := strings.Cut(part, " as "); ok {
					e.Callee, e.Qual = strings.TrimSpace(mod), strings.TrimSpace(alias)
				}
				g.Edges = append(g.Edges, e)
			}
			continue
		}
		if m := pyFrom.FindStringSubmatch(raw); m != nil {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: i + 1, Callee: m[1]})
			continue
		}
		if m := pyClass.FindStringSubmatch(raw); m != nil {
			scopeName := enclosingClass(stack)
			g.Symbols = append(g.Symbols, Symbol{Name: m[1], Kind: KindClass, Scope: scopeName,
				Line: i + 1, EndLine: blockEnd(lines, i, ind, false)})
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindClass})
			continue
		}
		if m := pyDef.FindStringSubmatch(raw); m != nil {
			kind, owner := KindFunc, ""
			if cls := enclosingClass(stack); cls != "" {
				kind, owner = KindMethod, cls
			}
			g.Symbols = append(g.Symbols, Symbol{Name: m[1], Kind: kind, Scope: owner,
				Line: i + 1, EndLine: blockEnd(lines, i, ind, false)})
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindFunc})
		}
	}
	return g
}

// enclosingClass is the innermost open block when it is a class, else "".
func enclosingClass(stack []scope) string {
	if len(stack) == 0 || stack[len(stack)-1].kind != KindClass {
		return ""
	}
	return stack[len(stack)-1].name
}
