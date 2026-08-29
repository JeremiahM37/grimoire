package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/JeremiahM37/grimoire/go/internal/auth"
)

// Accounts and spaces from the command line.
//
// The first account has to be creatable without one, and a server that is
// already listening is the wrong place to do it — the HTTP route that bootstraps
// the first administrator is open by necessity, so a deployment that means to be
// multi-user should close that window from the shell before anyone can reach the
// port. That is what `grimoire user add` is for.

func cmdUser(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire user add|list|passwd|map|unmap|identities …")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	accounts := e.auth

	switch args[0] {
	case "add":
		rest, admin := flagOut(args[1:], "--admin")
		if len(rest) == 0 {
			return fail("usage: grimoire user add NAME [--admin]")
		}
		name := rest[0]
		password, err := readPassword("password for " + name + ": ")
		if err != nil {
			return fail("%v", err)
		}
		confirm, err := readPassword("again: ")
		if err != nil {
			return fail("%v", err)
		}
		if password != confirm {
			return fail("passwords do not match")
		}
		role := auth.RoleMember
		if admin {
			role = auth.RoleAdmin
		}
		first := !accounts.Enabled()
		u, err := accounts.Create(name, strings.Join(rest[1:], " "), password, role)
		if err != nil {
			return fail("%v", err)
		}
		if _, err := accounts.EnsurePersonalSpace(u); err != nil {
			return fail("%v", err)
		}
		if err := restamp(e); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("created %s (%s)\n", u.Name, u.Role)
		if first {
			fmt.Println("this instance now requires sign-in; every other account " +
				"is created by an administrator")
		}
		return 0

	case "list":
		users, err := accounts.List()
		if err != nil {
			return fail("%v", err)
		}
		if len(users) == 0 {
			fmt.Println("no accounts — this instance is single-user")
			return 0
		}
		for _, u := range users {
			fmt.Printf("%-20s %-8s %s\n", u.Name, u.Role, u.Display)
		}
		return 0

	// A verified network identity names the caller truthfully and grants it
	// nothing. This is where an operator decides that a given tailnet login,
	// ZeroTier node or client certificate IS a particular account — once, on
	// purpose. Without a mapping the caller is attributed and stays anonymous,
	// which is the safe direction for an incomplete list.
	case "map":
		if len(args) < 4 {
			return fail("usage: grimoire user map BACKEND SUBJECT USER\n" +
				"  e.g. grimoire user map tailscale jam@github jam\n" +
				"  BACKEND is one of: tailscale, zerotier, mtls, proxy\n" +
				"  SUBJECT is what GET /api/identity reports, without the backend prefix")
		}
		u, err := accounts.ByName(args[3])
		if err != nil {
			return fail("%v", err)
		}
		if err := accounts.MapIdentity(args[1], args[2], u.ID); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%s identity %q signs in as %s\n", args[1], args[2], u.Name)
		return 0

	case "unmap":
		if len(args) < 3 {
			return fail("usage: grimoire user unmap BACKEND SUBJECT")
		}
		if err := accounts.UnmapIdentity(args[1], args[2]); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%s identity %q no longer signs in\n", args[1], args[2])
		return 0

	case "identities":
		ids, err := accounts.Identities()
		if err != nil {
			return fail("%v", err)
		}
		if len(ids) == 0 {
			fmt.Println("no identity mappings — verified callers are attributed but sign in as nobody")
			return 0
		}
		for _, id := range ids {
			fmt.Printf("%-12s %-30s %s\n", id.Source, id.External, id.Name)
		}
		return 0

	case "passwd":
		if len(args) < 2 {
			return fail("usage: grimoire user passwd NAME")
		}
		u, err := accounts.ByName(args[1])
		if err != nil {
			return fail("%v", err)
		}
		password, err := readPassword("new password for " + u.Name + ": ")
		if err != nil {
			return fail("%v", err)
		}
		if err := accounts.SetPassword(u.ID, password); err != nil {
			return fail("%v", err)
		}
		fmt.Println("password changed; existing sessions were ended")
		return 0
	}
	return fail("unknown user command %q", args[0])
}

func cmdSpace(args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire space add|list|member …")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	accounts := e.auth

	switch args[0] {
	case "add":
		if len(args) < 3 {
			return fail("usage: grimoire space add NAME PREFIX")
		}
		sp, err := accounts.CreateSpace(args[1], args[2], auth.KindShared, "")
		if err != nil {
			return fail("%v", err)
		}
		if err := restamp(e); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("created space %s covering %s\n", sp.Name, sp.Prefix)
		return 0

	case "list":
		spaces, err := accounts.Spaces()
		if err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%-24s %-24s %s\n", "NAME", "PREFIX", "KIND")
		for _, sp := range spaces {
			fmt.Printf("%-24s %-24s %s\n", sp.Name, sp.Prefix, sp.Kind)
		}
		fmt.Printf("%-24s %-24s %s\n", "Commons", "(everything else)", "commons")
		return 0

	case "member":
		rest, readOnly := flagOut(args[1:], "--read")
		if len(rest) < 2 {
			return fail("usage: grimoire space member SPACE_PREFIX USER [--read]")
		}
		sp, err := accounts.SpaceByPrefix(rest[0])
		if err != nil {
			return fail("no space covers %q", rest[0])
		}
		u, err := accounts.ByName(rest[1])
		if err != nil {
			return fail("%v", err)
		}
		role := auth.SpaceWriter
		if readOnly {
			role = auth.SpaceReader
		}
		if err := accounts.AddMember(sp.ID, u.ID, role); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("%s can now %s %s\n", u.Name, map[string]string{
			auth.SpaceReader: "read", auth.SpaceWriter: "read and write"}[role], sp.Prefix)
		return 0
	}
	return fail("unknown space command %q", args[0])
}

// restamp re-labels indexed rows after the space table changes, so retrieval
// filters on the new boundaries rather than the ones that existed at index time.
func restamp(e *env) error {
	spaces, err := e.auth.Spaces()
	if err != nil {
		return err
	}
	return e.index.RestampSpaces(func(path string) string {
		return auth.SpaceOf(path, spaces)
	})
}

// readPassword reads without echoing when it can. A pipe has no terminal, so
// scripted setup still works — and says so, rather than appearing to hang.
func readPassword(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(int(os.Stdin.Fd())) {
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return strings.TrimSpace(string(b)), err
	}
	line, err := stdin.ReadString('\n')
	fmt.Fprintln(os.Stderr, "(read from stdin, not a terminal — it was not hidden)")
	return strings.TrimSpace(line), err
}

// stdin is shared across reads. A fresh bufio.Reader per prompt buffers ahead
// and throws away what it did not use, so the second prompt of a scripted
// `user add` saw EOF instead of the confirmation line.
var stdin = bufio.NewReader(os.Stdin)

// flagOut removes one flag from an argument list and reports whether it was there.
func flagOut(args []string, flag string) ([]string, bool) {
	out := make([]string, 0, len(args))
	found := false
	for _, a := range args {
		if a == flag {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}
