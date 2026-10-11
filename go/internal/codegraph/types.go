// Package codegraph is a derived index of code symbols and the edges between
// them: where a name is defined, what calls it, what a file imports.
//
// It answers "where is X defined" and "who calls X" for a repository an agent
// is working in. It is deliberately NOT a type checker. Go files are parsed
// with go/parser, so declarations are exact, but call edges are matched by
// name: a call to `Save` is recorded as a call to "Save" whatever type it was
// made on. Callers results are therefore approximate, and say so. Python and
// TypeScript/JavaScript are read with line-oriented patterns, so they get
// declarations and imports but no call edges at all.
//
// Everything here is rebuildable from the source files. The store lives in the
// SQLite index next to the notes, and is keyed by repository root and file
// path, so a reindex only re-parses files whose content hash has changed.
package codegraph

// Symbol kinds. Kept as plain strings so they can be filtered on in SQL and
// shown to agents without a translation table.
const (
	KindFunc      = "func"
	KindMethod    = "method"
	KindStruct    = "struct"
	KindInterface = "interface"
	KindType      = "type"
	KindConst     = "const"
	KindVar       = "var"
	KindClass     = "class"
	KindEnum      = "enum"
)

// Edge kinds.
const (
	EdgeCall   = "call"
	EdgeImport = "import"
)

// Symbol is one named declaration in a file.
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Scope is the enclosing type for a method (the receiver type in Go, the
	// class in Python and TypeScript). Empty for package-level declarations.
	Scope   string `json:"scope,omitempty"`
	Line    int    `json:"line"`
	EndLine int    `json:"end_line,omitempty"`
}

// Qualified is the name a human would write: "Type.Method" for a method,
// the bare name otherwise.
func (s Symbol) Qualified() string {
	if s.Scope == "" {
		return s.Name
	}
	return s.Scope + "." + s.Name
}

// Confidence labels how a call edge was resolved to Target.
//
//   - resolved: the target was established by type information (Go) or by a
//     lexical rule that cannot be wrong about the declaration (self/this of an
//     enclosing class that defines the method).
//   - heuristic: the target is the likely one, from a name in this file or an
//     import, but shadowing or dynamic dispatch could make it another.
//   - unresolved: no target was established. Target is then empty, or names an
//     external package (importpath.Name) that is not in the repository.
const (
	ConfResolved   = "resolved"
	ConfHeuristic  = "heuristic"
	ConfUnresolved = "unresolved"
)

// Edge is one reference from a file. For a call, Caller is the qualified name
// of the enclosing function (empty for a call at package level) and Callee is
// the called identifier with Qual the identifier it was selected from
// (`pkg` in pkg.Save(), empty for a bare Save()). Target is the declaration the
// call reaches, written as "<file or package dir>#<Qualified>" for one in the
// repository (Go uses the package directory, so "#Save" is the root package),
// or as "<module>.<Name>" for a name outside it; Confidence says which. For an
// import, Callee is the import path (or module specifier), and Qual is the
// local alias if any; imports carry no Target or Confidence.
type Edge struct {
	Kind       string `json:"kind"`
	Line       int    `json:"line"`
	Caller     string `json:"caller,omitempty"`
	Callee     string `json:"callee"`
	Qual       string `json:"qual,omitempty"`
	Target     string `json:"target,omitempty"`
	Confidence string `json:"confidence,omitempty"`
}

// FileGraph is everything extracted from one source file.
type FileGraph struct {
	Lang    string
	Symbols []Symbol
	Edges   []Edge
}

// Languages the extractor understands, by file extension.
const (
	LangGo         = "go"
	LangPython     = "python"
	LangTypeScript = "typescript"
	LangJavaScript = "javascript"
)

// LangFor reports the language of a file by its extension, or "" when the
// extractor does not read that kind of file.
func LangFor(name string) string {
	switch {
	case hasSuffix(name, ".go"):
		return LangGo
	case hasSuffix(name, ".py"):
		return LangPython
	case hasSuffix(name, ".ts"), hasSuffix(name, ".tsx"):
		return LangTypeScript
	case hasSuffix(name, ".js"), hasSuffix(name, ".jsx"), hasSuffix(name, ".mjs"), hasSuffix(name, ".cjs"):
		return LangJavaScript
	}
	return ""
}

// Extract reads one file's source. name is only used to choose the language
// and to report positions; the file is not opened.
func Extract(name string, src []byte) (FileGraph, error) {
	switch LangFor(name) {
	case LangGo:
		return extractGo(name, src)
	case LangPython:
		return extractPython(name, src), nil
	case LangTypeScript, LangJavaScript:
		return extractScript(name, LangFor(name), src), nil
	}
	return FileGraph{}, errUnsupported
}

func hasSuffix(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
