package main

import (
	"strings"
	"testing"
)

// The agents listing is admin-only on the server, and the CLI is the same
// trust boundary as the vault it runs against. With no correction signals yet
// it must say so rather than print an empty table.
func TestMemoryAgentsOnAFreshVault(t *testing.T) {
	vaultDir(t)
	out, code := runCmd(t, "memory", "agents")
	if code != 0 {
		t.Fatalf("memory agents = %d: %s", code, out)
	}
	if !strings.Contains(out, "no agent has a correction signal yet") {
		t.Errorf("fresh vault output = %q", out)
	}
}
