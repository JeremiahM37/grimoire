package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

// Connecting accounts, and deciding on what agents ask to do with them.

var connectBooleans = map[string]bool{"no-browser": true, "no-connectors": true}

func connectFlags(args []string) ([]string, map[string]string) {
	// parseSecretFlags treats a bare flag followed by a word as key+value;
	// the booleans here must not swallow the provider name.
	for k := range connectBooleans {
		booleanFlags[k] = true
	}
	return parseSecretFlags(args)
}

func cmdConnect(args []string) int {
	rest, f := connectFlags(args)
	if len(rest) != 1 {
		return fail("usage: grimoire connect google|microsoft|slack|github [flags] (see grimoire help connect)")
	}
	provider := strings.ToLower(rest[0])
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	v := e.server.Secrets
	if err := unlockForCLI(v); err != nil {
		return fail("%v", err)
	}

	services := splitCSV(f["services"])
	allow := splitCSV(f["allow"])
	secretName := firstNonEmpty(f["secret"], provider+"-oauth")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	switch provider {
	case "google", "microsoft":
		if len(services) == 0 {
			services = map[string][]string{"google": {"gmail", "gdrive", "gcal"}, "microsoft": {"outlook", "onedrive"}}[provider]
		}
		clientID := firstNonEmpty(f["client-id"], os.Getenv("GRIMOIRE_"+strings.ToUpper(provider)+"_CLIENT_ID"))
		clientSecret := firstNonEmpty(f["client-secret"], os.Getenv("GRIMOIRE_"+strings.ToUpper(provider)+"_CLIENT_SECRET"))
		if clientID == "" {
			return fail("%s needs your own OAuth client: pass --client-id (and --client-secret for Google), "+
				"or set GRIMOIRE_%s_CLIENT_ID. docs/CONNECTORS.md walks through creating it.", provider, strings.ToUpper(provider))
		}
		scopes, err := connectors.ScopesFor(provider, services, allow)
		if err != nil {
			return fail("%v", err)
		}
		fmt.Println("Requesting these permissions (read-only unless you passed --allow):")
		for _, s := range scopes {
			fmt.Println("  -", s)
		}
		tok, err := connectors.LoopbackFlow(ctx, connectors.LoopbackOptions{
			Provider: provider, ClientID: clientID, ClientSecret: clientSecret,
			Tenant: f["tenant"], Scopes: scopes,
			Open: func(u string) error { return openBrowser(u, f["no-browser"] == "true") },
		})
		if err != nil {
			return fail("%v", err)
		}
		if err := v.Put(secretName, tok.Encode(), map[string]any{"kind": "oauth", "provider": provider}); err != nil {
			return fail("storing the token: %v", err)
		}
		fmt.Printf("Stored the %s token in the credential vault as %q (never shown or returned to agents).\n", provider, secretName)
		return provision(e, f, services, allow, secretName)
	case "slack", "github":
		token := firstNonEmpty(f["token"], os.Getenv("GRIMOIRE_CONNECT_TOKEN"))
		if tf := f["token-file"]; tf != "" {
			b, err := os.ReadFile(tf)
			if err != nil {
				return fail("%v", err)
			}
			token = strings.TrimSpace(string(b))
		}
		if provider == "github" && token == "" && f["client-id"] != "" {
			scope := firstNonEmpty(f["scope"], "public_repo")
			t, err := connectors.DeviceFlow(ctx, connectors.DeviceOptions{ClientID: f["client-id"], Scope: scope,
				Show: func(code, u string) {
					fmt.Printf("Open %s and enter the code %s\n", u, code)
				}})
			if err != nil {
				return fail("%v", err)
			}
			token = t
		}
		if token == "" {
			hint := map[string]string{
				"slack":  "Slack's redirect must be https, so there is no loopback flow: create the app (docs/CONNECTORS.md), install it, and paste its xoxp-/xoxb- token",
				"github": "paste a fine-grained personal access token (Issues: read; Contents: read; Issues: write only if you enable actions), or use --client-id for the device flow",
			}[provider]
			fmt.Fprintln(os.Stderr, hint)
			token, err = readPassword(provider + " token: ")
			if err != nil || token == "" {
				return fail("no token given")
			}
		}
		if err := v.Put(secretName, token, map[string]any{"kind": "token", "provider": provider}); err != nil {
			return fail("storing the token: %v", err)
		}
		fmt.Printf("Stored the %s token in the credential vault as %q.\n", provider, secretName)
		return provision(e, f, []string{provider}, allow, secretName)
	}
	return fail("unknown provider %q (google, microsoft, slack, github)", provider)
}

// provision creates one connector per service, read-only unless --allow names
// an action.
func provision(e *env, f map[string]string, services, allow []string, secretName string) int {
	if f["no-connectors"] == "true" {
		fmt.Println("No connectors created (--no-connectors). Create them with the console or POST /api/connectors.")
		return 0
	}
	store := e.server.Connectors
	for _, kind := range services {
		cfg := connectors.Config{}
		var actions []string
		for _, a := range allow {
			if k, name, ok := strings.Cut(a, "."); ok && k == kind {
				actions = append(actions, name)
			}
		}
		if len(actions) > 0 {
			cfg["actions"] = strings.Join(actions, ",")
		}
		if t := f["trust"]; t != "" {
			cfg["trust"] = t
		}
		switch kind {
		case "slack":
			cfg["channels"] = f["channels"]
			if f["post-channels"] != "" {
				cfg["post_channels"] = f["post-channels"]
			}
		case "github":
			cfg["repo"] = f["repo"]
		case "gmail":
			if l := f["labels"]; l != "" {
				cfg["labels"] = l
			}
		}
		if f["since"] != "" {
			cfg["since"] = f["since"]
		}
		c, err := connectors.Provision(store, kind, secretName, cfg, "")
		if err != nil {
			fmt.Fprintf(os.Stderr, "  %s: not created: %v\n", kind, err)
			continue
		}
		fmt.Printf("  %s: connector %s created (trust %s, actions: %s) — syncs hourly; `grimoire serve` runs it\n",
			kind, c.ID, connectors.TrustOf(c.Config), firstNonEmpty(cfg["actions"], "none, read-only"))
	}
	fmt.Println("Agents see these through the sources / source_search / source_read / source_act MCP tools.")
	return 0
}

func openBrowser(u string, printOnly bool) error {
	fmt.Println("Open this URL to approve access:\n\n  " + u + "\n")
	if printOnly {
		return nil
	}
	for _, bin := range []string{"xdg-open", "open"} {
		if p, err := exec.LookPath(bin); err == nil {
			_ = exec.Command(p, u).Start()
			break
		}
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func cmdSources(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	svc := e.server.Sources()
	if svc == nil {
		return fail("connectors are unavailable")
	}
	list, err := svc.Sources()
	if err != nil {
		return fail("%v", err)
	}
	if len(list) == 0 {
		fmt.Println("no sources connected. `grimoire connect google|microsoft|slack|github`")
		return 0
	}
	for _, s := range list {
		var acts []string
		for _, a := range s.Actions {
			if a.Enabled {
				acts = append(acts, a.Name+"("+a.Approval+")")
			}
		}
		fmt.Printf("%-16s %-9s %-22s trust=%-8s search=%v actions=%s\n", s.ID, s.Kind, s.Name, s.Trust, s.CanSearch,
			firstNonEmpty(strings.Join(acts, ","), "none"))
	}
	return 0
}

func cmdActions(args []string) int {
	rest, f := parseSecretFlags(args)
	sub := "list"
	if len(rest) > 0 {
		sub, rest = rest[0], rest[1:]
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	svc := e.server.Sources()
	if svc == nil {
		return fail("connectors are unavailable")
	}
	switch sub {
	case "list":
		state := connectorsPending
		if f["all"] == "true" {
			state = ""
		}
		list, err := svc.Actions(state, 100)
		if err != nil {
			return fail("%v", err)
		}
		if len(list) == 0 {
			fmt.Println("nothing waiting for approval")
		}
		for _, a := range list {
			fmt.Printf("%s  %-8s %s\n    agent=%s  %s\n", a.ID, a.State, a.Summary, a.Agent, a.Created)
			for k, v := range a.Params {
				fmt.Printf("      %s: %s\n", k, v)
			}
		}
		return 0
	case "audit":
		list, err := svc.Audit(100)
		if err != nil {
			return fail("%v", err)
		}
		for _, a := range list {
			fmt.Printf("%s  %-8s %-9s %-12s %-14s %s\n", a.TS, a.Kind, a.Op, a.Outcome, a.Agent, a.Detail)
		}
		return 0
	case "approve", "deny":
		if len(rest) != 1 {
			return fail("usage: grimoire actions %s ID [--note TEXT]", sub)
		}
		if sub == "approve" {
			if err := unlockForCLI(e.server.Secrets); err != nil {
				return fail("%v", err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		rec, err := svc.Decide(ctx, rest[0], sub == "approve", "owner (cli)", f["note"])
		if err != nil && rec.ID == "" {
			return fail("%v", err)
		}
		fmt.Printf("%s: %s", rec.ID, rec.State)
		if rec.Result != nil {
			fmt.Printf(" %s %s", rec.Result.Message, rec.Result.URL)
		}
		if rec.Error != "" {
			fmt.Printf(" (%s)", rec.Error)
		}
		fmt.Println()
		if rec.State == connectors.ActionFailed {
			return 1
		}
		return 0
	}
	return fail("usage: grimoire actions [list [--all] | audit | approve ID | deny ID [--note TEXT]]")
}

const connectorsPending = connectors.ActionPending
