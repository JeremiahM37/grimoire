package main

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/codegraph"
)

// cmdCode drives the code graph through the running server. Indexing goes
// through the server rather than the CLI opening the index itself for the
// same reason `dream` does: the server is the one place that decides which
// directories may be read (the vault and GRIMOIRE_CODE_ROOTS), and the admin
// token that gates these routes is held by the server's caller, not the vault.
func cmdCode(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire code index|symbol|callers|outline ... (see grimoire help code)")
	}
	sub, all := args[0], args[1:]
	// Connection flags (--url, --token) come from the bank client's parser;
	// the code subcommands read their own positionals below.
	f, err := parseBankFlags(all)
	if err != nil {
		return fail("%v", err)
	}
	rest, asJSON := flagOut(all, "--json")
	kind, _ := flagValue(rest, "--kind")
	root, _ := flagValue(rest, "--root")
	pos := positionals(rest)
	c := newBankClient(f)

	switch sub {
	case "index":
		if len(pos) != 1 {
			return fail("usage: grimoire code index PATH")
		}
		abs, err := filepath.Abs(pos[0])
		if err != nil {
			return fail("%v", err)
		}
		var st codegraph.Stats
		if err := c.do("POST", "/api/code/index", map[string]string{"path": abs}, &st); err != nil {
			return fail("%v", err)
		}
		if asJSON {
			return printCodeJSON(st)
		}
		fmt.Printf("%s: %d files, %d parsed, %d unchanged, %d removed, %d failed, %d over the size cap\n",
			st.Root, st.Files, st.Indexed, st.Unchanged, st.Removed, st.Failed, st.TooLarge)
		fmt.Printf("%d symbols, %d edges under this root\n", st.Symbols, st.Edges)
		return 0

	case "symbol":
		if len(pos) != 1 {
			return fail("usage: grimoire code symbol NAME [--kind K] [--root R] [--json]")
		}
		q := url.Values{"name": {pos[0]}}
		if kind != "" {
			q.Set("kind", kind)
		}
		if root != "" {
			q.Set("root", root)
		}
		var out struct {
			Symbols []codegraph.Hit `json:"symbols"`
		}
		if err := c.do("GET", "/api/code/symbol?"+q.Encode(), nil, &out); err != nil {
			return fail("%v", err)
		}
		if asJSON {
			return printCodeJSON(out)
		}
		if len(out.Symbols) == 0 {
			fmt.Println("no indexed symbol by that name")
			return 0
		}
		for _, h := range out.Symbols {
			fmt.Printf("%s\t%s\t%s:%d\n", h.Qualified, h.Kind, filepath.Join(h.Root, h.Path), h.Line)
		}
		return 0

	case "callers":
		if len(pos) != 1 {
			return fail("usage: grimoire code callers NAME [--json]")
		}
		var out struct {
			Callers []codegraph.Caller `json:"callers"`
		}
		if err := c.do("GET", "/api/code/callers?"+url.Values{"name": {pos[0]}}.Encode(), nil, &out); err != nil {
			return fail("%v", err)
		}
		if asJSON {
			return printCodeJSON(out)
		}
		if len(out.Callers) == 0 {
			fmt.Println("no indexed call to that name")
			return 0
		}
		fmt.Println("matched by name, not resolved: treat as leads to check")
		for _, cl := range out.Callers {
			from := cl.Caller
			if from == "" {
				from = "(package level)"
			}
			fmt.Printf("%s\t%s:%d\n", from, filepath.Join(cl.Root, cl.Path), cl.Line)
		}
		return 0

	case "outline":
		if len(pos) != 1 {
			return fail("usage: grimoire code outline FILE [--json]")
		}
		var out struct {
			Files []codegraph.FileOutline `json:"files"`
		}
		if err := c.do("GET", "/api/code/outline?"+url.Values{"file": {pos[0]}}.Encode(), nil, &out); err != nil {
			return fail("%v", err)
		}
		if asJSON {
			return printCodeJSON(out)
		}
		if len(out.Files) == 0 {
			fmt.Println("that file is not in the code index; run grimoire code index on its repository")
			return 0
		}
		for _, fo := range out.Files {
			fmt.Printf("%s (%s)\n", fo.Abs, fo.Lang)
			if fo.Error != "" {
				fmt.Printf("  not parsed: %s\n", fo.Error)
			}
			for _, im := range fo.Imports {
				fmt.Printf("  %4d  import %s\n", im.Line, im.Callee)
			}
			for _, s := range fo.Symbols {
				fmt.Printf("  %4d  %-9s %s\n", s.Line, s.Kind, s.Qualified())
			}
		}
		return 0
	}
	return fail("unknown code command %q: index, symbol, callers or outline", sub)
}

// codeValueFlags are the flags that take a value: this subcommand's own, and
// the connection flags the bank client reads.
var codeValueFlags = map[string]bool{
	"--kind": true, "--root": true, "--url": true, "--token": true, "--agent": true,
}

// positionals returns the arguments that are not flags or flag values.
func positionals(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			if codeValueFlags[a] && i+1 < len(args) {
				i++
			}
			continue
		}
		out = append(out, a)
	}
	return out
}

func printCodeJSON(v any) int {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fail("%v", err)
	}
	return 0
}
