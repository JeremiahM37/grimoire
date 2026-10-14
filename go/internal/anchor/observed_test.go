package anchor

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Observed hashes what a file-anchored probe saw: the verdict can stay the same
// while the observed value moves. That difference is what drift is built from.
func TestObservedFileChangesWithContentNotVerdict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(path, []byte("the router is at 192.168.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Anchor{Type: FileContains, Args: map[string]string{"path": path, "text": "192.168.0.1"}}
	first := Run(context.Background(), a)
	if first.Status != Pass {
		t.Fatalf("first run: %+v", first)
	}
	if !hex32.MatchString(first.Observed) {
		t.Fatalf("observed is not a 16-byte hex digest: %q", first.Observed)
	}
	if strings.Contains(first.Observed, "192") {
		t.Fatalf("observed leaked content: %q", first.Observed)
	}

	if err := os.WriteFile(path, []byte("the router is at 192.168.0.1 (moved notes)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := Run(context.Background(), a)
	if second.Status != Pass {
		t.Fatalf("second run: %+v", second)
	}
	if second.Observed == first.Observed {
		t.Fatalf("content changed but observed did not: %q", second.Observed)
	}

	// Same bytes, same digest: the hash is of content, not of the run.
	third := Run(context.Background(), a)
	if third.Observed != second.Observed {
		t.Fatalf("unchanged file gave two digests: %q vs %q", second.Observed, third.Observed)
	}
}

// file_exists hashes the file it confirms, and a verdict change still comes
// through as a status change, with the observed value empty on a miss.
func TestObservedFileExistsHashesAndMisses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "present.txt")
	if err := os.WriteFile(path, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := Anchor{Type: FileExists, Args: map[string]string{"path": path}}
	p := Run(context.Background(), a)
	if p.Status != Pass || !hex32.MatchString(p.Observed) {
		t.Fatalf("file_exists pass: %+v", p)
	}
	if err := os.WriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if q := Run(context.Background(), a); q.Status != Pass || q.Observed == p.Observed {
		t.Fatalf("file_exists after a change: %+v (before %q)", q, p.Observed)
	}
	os.Remove(path)
	m := Run(context.Background(), a)
	if m.Status != Fail || m.Observed != "" {
		t.Fatalf("file_exists miss should fail with no observed value: %+v", m)
	}
}

// A secret is never hashed: a digest of a low-entropy secret is a guessing
// oracle. The verdict still answers.
func TestObservedNeverHashesDeniedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".ai-secrets")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "key.yaml")
	if err := os.WriteFile(path, []byte("password: hunter2"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Anchor{Type: FileExists, Args: map[string]string{"path": path}})
	if res.Status != Pass {
		t.Fatalf("verdict changed: %+v", res)
	}
	if res.Observed != "" {
		t.Fatalf("a denied path was hashed: %q", res.Observed)
	}
}

// Probes with nothing to observe leave Observed empty.
func TestObservedEmptyWhenNothingToObserve(t *testing.T) {
	dir := t.TempDir()
	res := Run(context.Background(), Anchor{Type: DirExists, Args: map[string]string{"path": dir}})
	if res.Status != Pass || res.Observed != "" {
		t.Fatalf("dir_exists: %+v", res)
	}
}
