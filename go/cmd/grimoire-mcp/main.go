// Command grimoire-mcp exposes the substrate to agents over MCP (stdio).
//
// It is a thin client of the HTTP API rather than a second implementation, so
// the two surfaces cannot drift apart.
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/build"
	"github.com/JeremiahM37/grimoire/go/internal/mcp"
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
	base := os.Getenv("GRIMOIRE_URL")
	if base == "" {
		base = "http://127.0.0.1:" + envOr("GRIMOIRE_PORT", "9111")
	}
	srv := mcp.New(base, os.Getenv("GRIMOIRE_AGENT_NAME"))

	// stdio is the default because that is what local desktop agents speak.
	// The http transport is for web and hosted clients; it binds loopback,
	// since it carries no authentication of its own.
	if strings.EqualFold(os.Getenv("GRIMOIRE_MCP_TRANSPORT"), "http") {
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

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
