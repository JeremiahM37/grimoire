package mcp

import (
	"net/url"
)

// The code graph as tools: where a symbol is defined, who calls it, and what a
// file declares. The routes are admin-only (see internal/api/code_routes.go),
// so these answer only to a caller that holds the admin token.

func codeTools() []tool {
	return []tool{
		{
			Name: "code_symbol",
			Description: "Find where a name is defined in an indexed code repository: the " +
				"file, line and kind of every matching function, method, type, const or " +
				"class. Accepts a bare name (`Save`) or a qualified one (`Store.Save`). " +
				"Use this before reading source to find the right file.",
			InputSchema: obj(map[string]any{
				"name": strProp("the symbol's name, bare or qualified as Type.Method"),
				"kind": strProp("optional: func, method, struct, interface, type, const, var, class, enum"),
				"root": strProp("optional: restrict to one indexed repository root"),
			}, "name"),
		},
		{
			Name: "code_callers",
			Description: "List the call sites of a name in indexed code: the caller, file " +
				"and line. Matching is by name, not by resolved target, so a call to any " +
				"`Save` is listed under `Save`. Treat the result as a lead to verify, not a " +
				"complete or exact reference list. Python and TypeScript have no call edges.",
			InputSchema: obj(map[string]any{
				"name": strProp("the called name; a dotted pkg.Name narrows to calls written through pkg"),
			}, "name"),
		},
		{
			Name: "code_outline",
			Description: "Show what one indexed source file declares, in line order, with its " +
				"imports. Give an absolute path or a path relative to its repository root. " +
				"A cheap way to learn a file's shape before reading it.",
			InputSchema: obj(map[string]any{
				"file": strProp("the file's path, absolute or relative to its repository root"),
			}, "file"),
		},
	}
}

func (s *Server) dispatchCode(name string, args map[string]any) (result any, handled bool, err error) {
	switch name {
	case "code_symbol":
		q := url.Values{}
		q.Set("name", str(args, "name"))
		if k := str(args, "kind"); k != "" {
			q.Set("kind", k)
		}
		if r := str(args, "root"); r != "" {
			q.Set("root", r)
		}
		r, err := s.api("GET", "/api/code/symbol?"+q.Encode(), nil)
		return r, true, err
	case "code_callers":
		q := url.Values{}
		q.Set("name", str(args, "name"))
		r, err := s.api("GET", "/api/code/callers?"+q.Encode(), nil)
		return r, true, err
	case "code_outline":
		q := url.Values{}
		q.Set("file", str(args, "file"))
		r, err := s.api("GET", "/api/code/outline?"+q.Encode(), nil)
		return r, true, err
	}
	return nil, false, nil
}
