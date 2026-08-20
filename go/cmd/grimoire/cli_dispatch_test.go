package main

import (
	"strings"
	"testing"
)

// The help text and the command table must agree, in both directions.
//
// `grimoire backup` shipped in a release as an unreachable function: written,
// tested by calling it directly, documented in the usage text — and never added
// to the table that turns a word into a call. The tests could not see it
// because they WERE the caller. This compares the two lists and runs nothing,
// which is both the check that would have caught it and the only safe way to
// check a list that contains `restore`.
func isCommandWord(s string) bool {
	for _, r := range s {
		if !(r >= 'a' && r <= 'z') && r != '-' {
			return false
		}
	}
	return s != ""
}

func TestHelpTextAndCommandTableAgree(t *testing.T) {
	documented := map[string]bool{}
	for _, line := range strings.Split(usage, "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		// The banner line — "grimoire — local-first AI-native notes" — is not
		// a command, and neither is anything that is not a plain word.
		if len(fields) >= 2 && fields[0] == "grimoire" && isCommandWord(fields[1]) {
			documented[fields[1]] = true
		}
	}
	if len(documented) < 10 {
		t.Fatalf("found %d documented commands — is the usage text still a list?", len(documented))
	}
	// Handled before the table, so they are documented without being in it.
	for _, special := range []string{"serve", "version"} {
		delete(documented, special)
	}

	table := commands()
	for name := range documented {
		if _, ok := table[name]; !ok {
			t.Errorf("`grimoire %s` is in the help text but not in the command table", name)
		}
	}
	for name := range table {
		if !documented[name] {
			t.Errorf("`grimoire %s` dispatches but is not in the help text", name)
		}
	}
}
