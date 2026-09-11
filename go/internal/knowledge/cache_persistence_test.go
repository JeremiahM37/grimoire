package knowledge

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

type knowledgePersistenceFixture struct {
	root string
	ix   *index.Index
	v    *vault.Vault
	db   interface{ Close() error }
}

func newKnowledgePersistenceFixture(t *testing.T, notes map[string]string) knowledgePersistenceFixture {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ix := index.New(database, v, embed.Hash{})
	for path, body := range notes {
		if _, err := v.Write(path, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	return knowledgePersistenceFixture{root: root, ix: ix, v: v, db: database}
}

func persistenceCompleter(calls *atomic.Int32) func(string) (string, error) {
	return func(prompt string) (string, error) {
		calls.Add(1)
		if strings.Contains(prompt, "Alpha") {
			return `[{"subject":"Alpha","relation":"owns","object":"Apple","quote":"Alpha owns Apple."}]`, nil
		}
		return `[{"subject":"Beta","relation":"owns","object":"Banana","quote":"Beta owns Banana."}]`, nil
	}
}

func TestExtractionPersistenceFailureDoesNotPopulateMemoryCache(t *testing.T) {
	fixture := newKnowledgePersistenceFixture(t, map[string]string{
		"note.md": "# Note\n\nAlpha owns Apple.",
	})
	var calls atomic.Int32
	k := New(fixture.ix, fixture.v)
	k.SetCompleterVersion("model-1", persistenceCompleter(&calls))

	cachePath := filepath.Join(fixture.root, ".grimoire", "knowledge-triples.json")
	if err := os.Mkdir(cachePath, 0o700); err != nil {
		t.Fatal(err)
	}
	first := k.Extract([]string{"note.md"}, false, nil)
	if len(first) != 1 || first[0].Status != "error" {
		t.Fatalf("forced persistence failure = %+v", first)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("first extraction calls = %d, want 1", got)
	}
	if err := os.Remove(cachePath); err != nil {
		t.Fatal(err)
	}
	second := k.Extract([]string{"note.md"}, false, nil)
	if len(second) != 1 || second[0].Status != "indexed" || second[0].Triples != 1 {
		t.Fatalf("retry after persistence recovery = %+v", second)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("retry calls = %d, want 2; failed persist may have populated cache", got)
	}
}

func TestConcurrentDocumentExtractionPersistsBothAcrossRestart(t *testing.T) {
	fixture := newKnowledgePersistenceFixture(t, map[string]string{
		"alpha.md": "# Alpha\n\nAlpha owns Apple.",
		"beta.md":  "# Beta\n\nBeta owns Banana.",
	})
	var calls atomic.Int32
	complete := persistenceCompleter(&calls)
	k := New(fixture.ix, fixture.v)
	k.SetCompleterVersion("model-1", complete)

	results := make(chan []ExtractResult, 2)
	var wg sync.WaitGroup
	for _, path := range []string{"alpha.md", "beta.md"} {
		wg.Add(1)
		go func(path string) {
			defer wg.Done()
			results <- k.Extract([]string{path}, false, nil)
		}(path)
	}
	wg.Wait()
	close(results)
	for result := range results {
		if len(result) != 1 || result[0].Status != "indexed" || result[0].Triples != 1 {
			t.Fatalf("concurrent extraction = %+v", result)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("concurrent model calls = %d, want 2", got)
	}

	restarted := New(fixture.ix, fixture.v)
	restarted.SetCompleterVersion("model-1", complete)
	for _, path := range []string{"alpha.md", "beta.md"} {
		result := restarted.Extract([]string{path}, false, nil)
		if len(result) != 1 || result[0].Status != "cached" || result[0].Triples != 1 {
			t.Fatalf("restart cache for %s = %+v", path, result)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("restart invoked model %d additional times", got-2)
	}
}
