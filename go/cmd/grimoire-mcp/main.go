// Command grimoire-mcp exposes the substrate to agents over MCP (stdio).
//
// It is a thin client of the HTTP API rather than a second implementation, so
// the two surfaces cannot drift apart.
package main

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/build"
	"github.com/JeremiahM37/grimoire/go/internal/identity"
	"github.com/JeremiahM37/grimoire/go/internal/mcp"
	"github.com/JeremiahM37/grimoire/go/internal/oauth"
)

func main() {
	// An agent client that cannot be asked what it is makes every "which
	// build?" question a guess. stdio servers are launched by other programs,
	// so this is the only way to ask one.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println("grimoire-mcp " + build.String())
			return
		}
	}
	base := os.Getenv(mcp.EnvURL)
	if base == "" {
		base = "http://127.0.0.1:" + envOr("GRIMOIRE_PORT", "9111")
	}
	srv := mcp.New(base, os.Getenv(mcp.EnvAgentName))

	oa, err := oauthFromEnv(srv.AdminToken)
	if err != nil {
		fmt.Fprintln(os.Stderr, "grimoire-mcp: OAuth configuration:", err)
		os.Exit(1)
	}
	srv.OAuth = oa

	// stdio is the default because that is what local desktop agents speak.
	// The http transport is for web and hosted clients; it binds loopback,
	// since it carries no authentication of its own beyond GRIMOIRE_MCP_TOKEN
	// or OAuth.
	if strings.EqualFold(os.Getenv("GRIMOIRE_MCP_TRANSPORT"), "http") {
		if oa != nil {
			go serveAuthorize(oa)
		}
		addr := envOr("GRIMOIRE_MCP_ADDR", "127.0.0.1:"+envOr("GRIMOIRE_MCP_PORT", "9112"))
		fmt.Fprintf(os.Stderr, "grimoire-mcp: serving mcp over http at http://%s/mcp\n", addr)
		if err := srv.ListenAndServe(addr); err != nil {
			fmt.Fprintln(os.Stderr, "grimoire-mcp:", err)
			os.Exit(1)
		}
		return
	}
	if err := srv.Serve(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "grimoire-mcp:", err)
		os.Exit(1)
	}
}

// oauthFromEnv builds the OAuth authorization server this process's public
// listener uses, or nil — the default — when GRIMOIRE_PUBLIC_BASE is unset.
//
// OAuth is off unless BOTH GRIMOIRE_PUBLIC_BASE and
// GRIMOIRE_OAUTH_AUTHORIZE_BASE are set, the same "named explicitly or
// inert" rule internal/identity's FromEnv already uses: a deployment that
// never heard of claude.ai or ChatGPT connectors gets no new attack surface
// just because a binary was upgraded.
func oauthFromEnv(adminToken string) (*oauth.Handler, error) {
	publicBase := strings.TrimSpace(os.Getenv("GRIMOIRE_PUBLIC_BASE"))
	authorizeBase := strings.TrimSpace(os.Getenv("GRIMOIRE_OAUTH_AUTHORIZE_BASE"))
	if publicBase == "" && authorizeBase == "" {
		return nil, nil
	}
	if publicBase == "" || authorizeBase == "" {
		return nil, fmt.Errorf(
			"GRIMOIRE_PUBLIC_BASE and GRIMOIRE_OAUTH_AUTHORIZE_BASE must both be set to enable OAuth " +
				"(one was set without the other) — see docs/web-connectors.md")
	}

	dbPath := strings.TrimSpace(os.Getenv("GRIMOIRE_OAUTH_DB"))
	if dbPath == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("no GRIMOIRE_OAUTH_DB and could not find a home directory: %w", err)
		}
		dbPath = filepath.Join(home, ".grimoire-mcp", "oauth.db")
	}
	store, err := oauth.Open(dbPath)
	if err != nil {
		return nil, fmt.Errorf("opening OAuth store at %s: %w", dbPath, err)
	}

	var redirects *oauth.RedirectAllowlist
	if raw := strings.TrimSpace(os.Getenv("GRIMOIRE_OAUTH_ALLOWED_REDIRECTS")); raw != "" {
		redirects = oauth.NewRedirectAllowlist(strings.Split(raw, ","))
	}

	var allowedLogins []string
	if raw := strings.TrimSpace(os.Getenv("GRIMOIRE_OAUTH_ALLOWED_LOGINS")); raw != "" {
		allowedLogins = strings.Split(raw, ",")
	}

	// Reuses the same resolver construction cmd/grimoire itself uses — off
	// unless GRIMOIRE_IDENTITY names a backend, normally "tailscale".
	return oauth.New(oauth.Config{
		Store:         store,
		PublicBase:    publicBase,
		AuthorizeBase: authorizeBase,
		Redirects:     redirects,
		Identity:      identity.FromEnv(),
		AllowedLogins: allowedLogins,
		AdminToken:    adminToken,
	})
}

// serveAuthorize runs the private, owner-only consent listener. It is a
// second HTTP server on a second address on purpose — see internal/oauth's
// package doc and docs/web-connectors.md for why /oauth/authorize must never
// share the public listener's address, and why binding it is this process's
// job rather than something the public HTTPHandler could gate with a check.
//
// This listener speaks plain HTTP. TLS is deliberately not this process's
// job either: `tailscale serve` already terminates HTTPS for other services
// on this estate (see docs/web-connectors.md), which means grimoire-mcp
// never needs to load a certificate, and GRIMOIRE_OAUTH_AUTHORIZE_BASE is
// what a browser actually loads. Binding this to anything other than
// loopback or a tailnet-only address defeats the whole reason the surface is
// separate.
func serveAuthorize(oa *oauth.Handler) {
	addr := envOr("GRIMOIRE_OAUTH_AUTHORIZE_ADDR", "127.0.0.1:9115")
	fmt.Fprintf(os.Stderr, "grimoire-mcp: serving the OAuth consent page at http://%s/oauth/authorize "+
		"(reachable to the outside only via GRIMOIRE_OAUTH_AUTHORIZE_BASE, e.g. through `tailscale serve`)\n", addr)
	srv := &http.Server{
		Addr:              addr,
		Handler:           oa.PrivateMux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       time.Minute,
		WriteTimeout:      time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, "grimoire-mcp: OAuth consent listener:", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
