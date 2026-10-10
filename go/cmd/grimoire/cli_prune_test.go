package main

import (
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// withClock runs fn with the clock at when, so a fact can be written as if it
// were months old. Eviction is an age rule, so the CLI tests need an old fact.
func withClock(t *testing.T, when time.Time, fn func()) {
	t.Helper()
	old := vault.Now
	vault.Now = func() time.Time { return when }
	defer func() { vault.Now = old }()
	fn()
}

func TestRememberTakesAnImportance(t *testing.T) {
	dir := vaultDir(t)
	if out, code := runCmd(t, "remember", "the backup key is in the vault", "--topic", "ops", "--importance", "5"); code != 0 {
		t.Fatalf("remember = %d: %s", code, out)
	}
	if body := memoryNote(t, dir, "ops.md"); !strings.Contains(body, "imp=5") {
		t.Errorf("importance not written:\n%s", body)
	}
}

func TestRememberRejectsANonNumericImportance(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "remember", "a fact", "--topic", "ops", "--importance", "high"); code == 0 {
		t.Error("a non-numeric importance was accepted")
	}
}

func TestPruneIsADryRunUntilApplied(t *testing.T) {
	dir := vaultDir(t)
	old := time.Now().Add(-200 * 24 * time.Hour)
	withClock(t, old, func() {
		if out, code := runCmd(t, "remember", "the mailer is postfix", "--topic", "ops", "--importance", "1"); code != 0 {
			t.Fatalf("remember = %d: %s", code, out)
		}
	})
	before := memoryNote(t, dir, "ops.md")

	out, code := runCmd(t, "memory", "prune")
	if code != 0 || !strings.Contains(out, "dry run") || !strings.Contains(out, "the mailer is postfix") {
		t.Fatalf("dry run = %d: %s", code, out)
	}
	if after := memoryNote(t, dir, "ops.md"); after != before {
		t.Fatalf("dry run changed the note")
	}

	out, code = runCmd(t, "memory", "prune", "--apply")
	if code != 0 || !strings.Contains(out, "retracted 1 of 1") {
		t.Fatalf("apply = %d: %s", code, out)
	}
	if body := memoryNote(t, dir, "ops.md"); !strings.Contains(body, "retracted:memory-prune") {
		t.Errorf("apply did not retract through the forget path:\n%s", body)
	}
}

func TestPruneRefusesToDoBothModes(t *testing.T) {
	vaultDir(t)
	if _, code := runCmd(t, "memory", "prune", "--apply", "--dry-run"); code == 0 {
		t.Error("--apply with --dry-run was accepted")
	}
}
