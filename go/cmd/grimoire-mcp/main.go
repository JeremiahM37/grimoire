// Command grimoire-mcp exposes the substrate to agents over MCP (stdio).
//
// It is a thin client of the HTTP API rather than a second implementation, so
// the two surfaces cannot drift apart.
package main

import (
	"fmt"
	"os"

	"github.com/JeremiahM37/grimoire/go/internal/mcp"
)

func main() {
	base := os.Getenv("GRIMOIRE_URL")
	if base == "" {
		base = "http://127.0.0.1:" + envOr("GRIMOIRE_PORT", "9111")
	}
	srv := mcp.New(base, os.Getenv("GRIMOIRE_AGENT_NAME"))
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
