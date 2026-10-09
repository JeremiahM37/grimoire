package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/secrets/providers"
)

// External password managers, from the shell.
//
//	grimoire secret provider add NAME --kind K [--set k=v]… [--secret k]… [--secret-env k=ENV]… [--secret-file k=PATH]…
//	grimoire secret provider list | test NAME | remove NAME | kinds
//	grimoire secret link HANDLE REF [--provider NAME] [--note TEXT]
//	grimoire secret unlink HANDLE
//	grimoire secret import --from bitwarden --folder X [--provider NAME] [--item ID]… [--prefix P] [--yes]
//
// Unlock material is read from a hidden prompt, an environment variable or a
// file — never from an argument, which would sit in shell history and /proc.

func cmdSecretProvider(v *secrets.Vault, args []string) int {
	if len(args) == 0 {
		return fail("usage: grimoire secret provider add|list|test|remove|kinds …")
	}
	switch args[0] {
	case "kinds":
		for _, k := range providers.Kinds() {
			fmt.Printf("%-13s %-10s %s\n", k.Name, k.Scheme+"://", k.Summary)
			for _, f := range k.Settings {
				fmt.Printf("    --set %-16s %s\n", f.Name, f.Help)
			}
			for _, f := range k.Secrets {
				req := ""
				if f.Required {
					req = " (required)"
				}
				fmt.Printf("    secret %-15s %s%s\n", f.Name, f.Help, req)
			}
		}
		return 0

	case "list", "ls":
		ps, err := v.Providers()
		if err != nil {
			return fail("%v", err)
		}
		if len(ps) == 0 {
			fmt.Println("no providers configured; `grimoire secret provider kinds` lists what is supported")
			return 0
		}
		for _, p := range ps {
			keys := make([]string, 0, len(p.Settings))
			for k, val := range p.Settings {
				keys = append(keys, k+"="+val)
			}
			sort.Strings(keys)
			fmt.Printf("%-16s %-12s %d link(s)  unlock: %s  %s\n", p.Name, p.Kind, p.Links,
				orDash(strings.Join(p.SecretKeys, ",")), strings.Join(keys, " "))
		}
		return 0

	case "test":
		if len(args) < 2 {
			return fail("usage: grimoire secret provider test NAME")
		}
		msg, err := v.TestProvider(args[1])
		if err != nil {
			fmt.Printf("%s: NOT USABLE — %v\n", args[1], err)
			return 1
		}
		fmt.Printf("%s: ok — %s\n", args[1], msg)
		return 0

	case "remove", "rm":
		if len(args) < 2 {
			return fail("usage: grimoire secret provider remove NAME")
		}
		if err := v.RemoveProvider(args[1]); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("removed %s and its stored unlock material\n", args[1])
		return 0

	case "add":
		return providerAdd(v, args[1:])
	}
	return fail("unknown: grimoire secret provider %s", args[0])
}

func providerAdd(v *secrets.Vault, args []string) int {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return fail("usage: grimoire secret provider add NAME --kind KIND [--set k=v] [--secret k] [--secret-env k=ENV] [--secret-file k=PATH]")
	}
	cfg := providers.Config{Name: args[0], Settings: map[string]string{}, Secrets: map[string]string{}}
	var prompt []string
	for i := 1; i < len(args); i++ {
		flag := args[i]
		if i+1 >= len(args) {
			return fail("%s needs a value", flag)
		}
		val := args[i+1]
		i++
		switch flag {
		case "--kind":
			cfg.Kind = val
		case "--set":
			k, x, ok := strings.Cut(val, "=")
			if !ok {
				return fail("--set wants key=value, got %q", val)
			}
			cfg.Settings[k] = x
		case "--secret":
			prompt = append(prompt, val)
		case "--secret-env":
			k, env, ok := strings.Cut(val, "=")
			if !ok || os.Getenv(env) == "" {
				return fail("--secret-env %s: variable %q is empty or unset", val, env)
			}
			cfg.Secrets[k] = os.Getenv(env)
		case "--secret-file":
			k, path, ok := strings.Cut(val, "=")
			if !ok {
				return fail("--secret-file wants key=path, got %q", val)
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return fail("%v", err)
			}
			cfg.Secrets[k] = strings.TrimSpace(string(raw))
		default:
			return fail("unknown flag %s", flag)
		}
	}
	kind := providers.KindByName(cfg.Kind)
	if kind == nil {
		return fail("--kind is required; `grimoire secret provider kinds` lists them")
	}
	for _, f := range kind.Secrets {
		if f.Required && cfg.Secrets[f.Name] == "" {
			prompt = append(prompt, f.Name)
		}
	}
	for _, name := range prompt {
		if cfg.Secrets[name] != "" {
			continue
		}
		val, err := readPassword(name + " for " + cfg.Name + ": ")
		if err != nil {
			return fail("%v", err)
		}
		if val == "" {
			return fail("empty value for %s", name)
		}
		cfg.Secrets[name] = val
	}
	if err := v.AddProvider(cfg); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("provider %s (%s) stored; unlock material is sealed in the vault.\n", cfg.Name, cfg.Kind)
	fmt.Printf("check it:  grimoire secret provider test %s\n", cfg.Name)
	return 0
}

func cmdSecretLink(v *secrets.Vault, args []string) int {
	rest, flags := parseSecretFlags(args)
	if len(rest) != 2 {
		return fail("usage: grimoire secret link HANDLE PROVIDER-REF [--provider NAME] [--note TEXT]\n" +
			"  e.g. grimoire secret link github bitwarden://<item-id>/password")
	}
	handle, ref := rest[0], rest[1]
	prov, err := v.ResolveProviderName(ref, flags["provider"])
	if err != nil {
		return fail("%v", err)
	}
	if err := v.Link(handle, prov, ref, flags["note"]); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("%s now points at %s via %s. No value is stored; it is fetched when a grant is used.\n", handle, ref, prov)
	return 0
}

func cmdSecretUnlink(v *secrets.Vault, args []string) int {
	if len(args) < 1 {
		return fail("usage: grimoire secret unlink HANDLE")
	}
	if err := v.Unlink(args[0]); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("unlinked %s (the item in your password manager is untouched)\n", args[0])
	return 0
}

// importFromProvider copies selected items INTO Grimoire's own vault. It is
// the opposite of linking — the value leaves the manager — so it is explicit,
// confirms each item, and is never run by anything but this command.
func importFromProvider(v *secrets.Vault, args []string) int {
	var from, folder, provider, prefix string
	var only []string
	yes := false
	for i := 0; i < len(args); i++ {
		switch a := args[i]; a {
		case "--yes":
			yes = true
		case "--from", "--folder", "--provider", "--prefix", "--item":
			if i+1 >= len(args) {
				return fail("%s needs a value", a)
			}
			i++
			switch a {
			case "--from":
				from = args[i]
			case "--folder":
				folder = args[i]
			case "--provider":
				provider = args[i]
			case "--prefix":
				prefix = args[i]
			case "--item":
				only = append(only, args[i])
			}
		default:
			return fail("unknown flag %s", a)
		}
	}
	if from == "" || folder == "" {
		return fail("usage: grimoire secret import --from KIND --folder NAME [--provider NAME] [--item ID]… [--prefix P] [--yes]")
	}
	kind := providers.KindByName(from)
	if kind == nil {
		return fail("unknown --from %q", from)
	}
	if provider == "" {
		name, err := v.ResolveProviderName(kind.Scheme+"://x/y", "")
		if err != nil {
			return fail("%v", err)
		}
		provider = name
	}
	items, err := v.ListProviderItems(provider, folder)
	if err != nil {
		return fail("%v", err)
	}
	if len(only) > 0 {
		keep := map[string]bool{}
		for _, id := range only {
			keep[id] = true
		}
		var sel []providers.Item
		for _, it := range items {
			if keep[it.ID] {
				sel = append(sel, it)
			}
		}
		items = sel
	}
	if len(items) == 0 {
		fmt.Println("nothing to import")
		return 0
	}
	if prefix == "" {
		prefix = provider + "/"
	}
	n := 0
	for _, it := range items {
		handle := prefix + importSlug(it.Name)
		if !yes {
			ans, _ := readLine(fmt.Sprintf("copy %q into the vault as %s? [y/N] ", it.Name, handle))
			if a := strings.ToLower(strings.TrimSpace(ans)); a != "y" && a != "yes" {
				continue
			}
		}
		val, err := v.ResolveRef(provider, it.Ref)
		if err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", it.Name, err)
			continue
		}
		note := "imported from " + from + " folder " + folder
		if err := v.PutVersioned(handle, val, map[string]any{secrets.MetaNote: note}, note); err != nil {
			fmt.Fprintf(os.Stderr, "skipped %s: %v\n", it.Name, err)
			continue
		}
		fmt.Printf("imported %s\n", handle)
		n++
	}
	fmt.Printf("%d item(s) copied. They are now ordinary Grimoire secrets; the originals are untouched.\n", n)
	return 0
}

func importSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func readLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	return stdin.ReadString('\n')
}
