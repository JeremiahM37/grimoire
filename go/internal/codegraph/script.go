package codegraph

import (
	"regexp"
	"strings"
)

// JavaScript and TypeScript are read with patterns, the same way Python is.
// Blocks are tracked by indentation, and braces are not counted. Both are fine
// for the formatting real projects use, and both are wrong for minified
// one-liners, which the size cap mostly keeps out.
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
	jsExport  = regexp.MustCompile(`^\s*export\s+(?:type\s+)?[^'"]*?\bfrom\s+['"]([^'"]+)['"]`)
	jsRequire = regexp.MustCompile(`require\(\s*['"]([^'"]+)['"]\s*\)`)
	jsCloser  = regexp.MustCompile(`^\s*\}\s*from\s+['"]([^'"]+)['"]`)
)

// jsKeywords are words that look like a method call inside a class body
// (`if (x) {`) but are not declarations.
var jsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "catch": true,
	"function": true, "return": true, "with": true, "do": true,
}

func extractScript(lang string, src []byte) FileGraph {
	lines := splitLines(src)
	g := FileGraph{Lang: lang}
	var stack []scope
	add := func(name, kind string, line, ind int, braces bool) {
		g.Symbols = append(g.Symbols, Symbol{Name: name, Kind: kind, Scope: enclosingClass(stack),
			Line: line, EndLine: blockEnd(lines, line-1, ind, braces)})
	}
	for i, raw := range lines {
		t := strings.TrimSpace(raw)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		ind := indentOf(raw)
		stack = popTo(stack, ind)
		line := i + 1

		if m := jsImport.FindStringSubmatch(raw); m != nil {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
			continue
		}
		if m := jsExport.FindStringSubmatch(raw); m != nil {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
			continue
		}
		if m := jsCloser.FindStringSubmatch(raw); m != nil {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
			continue
		}
		if m := jsRequire.FindStringSubmatch(raw); m != nil && strings.Contains(raw, "= require(") {
			g.Edges = append(g.Edges, Edge{Kind: EdgeImport, Line: line, Callee: m[1]})
			continue
		}
		if m := jsClass.FindStringSubmatch(raw); m != nil {
			add(m[1], KindClass, line, ind, true)
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindClass})
			continue
		}
		if m := jsIface.FindStringSubmatch(raw); m != nil {
			add(m[1], KindInterface, line, ind, true)
			continue
		}
		if m := jsEnum.FindStringSubmatch(raw); m != nil {
			add(m[1], KindEnum, line, ind, true)
			continue
		}
		if m := jsType.FindStringSubmatch(raw); m != nil {
			add(m[1], KindType, line, ind, false)
			continue
		}
		if m := jsFunc.FindStringSubmatch(raw); m != nil {
			add(m[1], KindFunc, line, ind, true)
			stack = append(stack, scope{indent: ind, name: m[1], kind: KindFunc})
			continue
		}
		if m := jsVar.FindStringSubmatch(raw); m != nil && len(stack) == 0 {
			kind := KindVar
			if m[1] == "const" {
				kind = KindConst
			}
			if jsArrow.MatchString(strings.TrimSpace(m[3])) {
				kind = KindFunc
			}
			add(m[2], kind, line, ind, true)
			// A function body, or an object literal, opens a block that holds
			// its own local declarations; those are not top-level.
			if kind == KindFunc || strings.HasSuffix(t, "{") {
				stack = append(stack, scope{indent: ind, name: m[2], kind: kind})
			}
			continue
		}
		if enclosingClass(stack) != "" && strings.HasSuffix(t, "{") {
			if m := jsMethod.FindStringSubmatch(raw); m != nil && !jsKeywords[m[1]] {
				add(m[1], KindMethod, line, ind, true)
			}
		}
	}
	return g
}
