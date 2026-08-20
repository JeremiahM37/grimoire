package sync

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// A conflict copy exists to preserve a version that lost a race. Two devices
// losing the same race inside the same second would give both copies the same
// name — so the mechanism that exists to preserve a version would destroy one.
func TestConflictNamesDoNotCollideWithinASecond(t *testing.T) {
	old := vault.Now
	vault.Now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { vault.Now = old })

	taken := map[string]bool{}
	names := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		name := conflictNameUnless("notes/plan.md", func(c string) bool { return taken[c] })
		if taken[name] {
			t.Fatalf("name %q reused — a conflict copy would overwrite another", name)
		}
		taken[name] = true
		names = append(names, name)
	}
	if names[0] != "notes/plan (conflict 20260820-120000).md" {
		t.Errorf("first conflict name = %q", names[0])
	}
	for _, n := range names {
		if len(n) < 10 || n[len(n)-3:] != ".md" {
			t.Errorf("conflict name is not a note path: %q", n)
		}
	}
}
