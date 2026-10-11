package api

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

// Write-time verification over HTTP. Each kind of reference is tried twice: a
// real one must verify and keep the observed basis; a made-up one must be kept
// as claimed, marked unverified, and read as inferred.

const realDocHash = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestPulledURLVerifiesOnlyWhenTheStoreFetchedIt(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	fm := markdown.NewFrontmatter()
	fm.Set("origin", "web:example.org")
	if _, err := s.Vault.Write("pulled/runbook.md",
		"Backups run at 03:00. Source: https://example.org/runbook\n", fm); err != nil {
		t.Fatal(err)
	}
	// The origin is read from the index, so the note must be indexed first.
	if _, err := s.Index.Reindex(); err != nil {
		t.Fatal(err)
	}

	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the backup runs at 03:00", "evidence": []string{"https://example.org/runbook"}})
	if b := basisOf(t, recallFacts(t, h, "?q=backup"), "backup runs"); b != "observed" {
		t.Fatalf("a URL the store fetched read as %q, want observed", b)
	}

	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the deploy runs at 04:00", "evidence": []string{"https://example.net/never-fetched"}})
	if b := basisOf(t, recallFacts(t, h, "?q=deploy"), "deploy runs"); b != "inferred" {
		t.Fatalf("a URL the store never fetched read as %q, want inferred", b)
	}
}

func TestStoredDocumentHashVerifies(t *testing.T) {
	t.Parallel()
	s, h := testServer(t)
	body := []byte("the rollout needs approval from ops\n")
	if _, err := s.Documents.ImportBytes("upload:memo", "memo.txt", body); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	hex := fmt.Sprintf("%x", sum)

	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the rollout needs approval", "evidence": []string{"sha256:" + hex}})
	if b := basisOf(t, recallFacts(t, h, "?q=rollout"), "rollout needs"); b != "observed" {
		t.Fatalf("a stored document's hash read as %q, want observed", b)
	}

	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the rollout skips approval", "evidence": []string{"sha256:" + realDocHash}})
	if b := basisOf(t, recallFacts(t, h, "?q=rollout"), "rollout skips"); b != "inferred" {
		t.Fatalf("a hash no stored document has read as %q, want inferred", b)
	}
}

func TestEntryIdEvidenceVerifiesOnlyForAnExistingEntry(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	src := remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the cache is redis"})
	id, _ := src["id"].(string)
	if id == "" {
		t.Fatalf("remember returned no id: %v", src)
	}
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the session store is redis", "evidence": []string{id}})
	body := noteBody(t, h, "memory/ops.md")
	if !strings.Contains(body, "evv=verified") {
		t.Fatalf("an existing entry id did not verify:\n%s", body)
	}

	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the queue is nats", "evidence": []string{"0123456789ab"}})
	body = noteBody(t, h, "memory/ops.md")
	if strings.Count(body, "evv=unverified") != 1 {
		t.Fatalf("a made-up entry id should be unverified:\n%s", body)
	}
}

// The explain view is the surface a person reads a basis from, so the verdict
// must reach it, not only the trailer.
func TestExplainShowsTheVerificationVerdict(t *testing.T) {
	t.Parallel()
	_, h := testServer(t)
	remember(t, h, map[string]any{"topic": "ops", "infer": false,
		"text": "the backup runs at 03:00", "evidence": []string{"memory/never-written.md"}})
	w := do(t, h, "GET", "/api/memory?q=backup&explain=1", nil)
	if w.Code != 200 {
		t.Fatalf("recall = %d: %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), `"verification":"unverified"`) {
		t.Fatalf("explain omitted the verification verdict: %s", w.Body)
	}
}
