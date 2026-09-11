package knowledge

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

func TestQueryAddsConnectedBridgeEvidenceOnlyAtDepth(t *testing.T) {
	root := t.TempDir()
	store, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ix := index.New(store, v, embed.Hash{})
	if _, err = v.Write("seed.md", "# Seed\n\nalpha [[bridge]]", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Write("bridge.md", "# Bridge\n\nbridge evidence for deployment", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	k := New(ix, v)
	result, err := k.Query("alpha", 1, index.Filter{}, nil, "", "", 2, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range result.Citations {
		if c.Path == "bridge.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("depth 2 omitted bridge citation: %+v", result.Citations)
	}
	result, err = k.Query("alpha", 1, index.Filter{}, nil, "", "", 0, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range result.Citations {
		if c.Path == "bridge.md" {
			t.Fatal("depth 0 included bridge citation")
		}
	}
}

func TestExtractionCacheIsDurableAndGraphDoesNotCallModel(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	ix := index.New(database, v, embed.Hash{})
	if _, err = v.Write("note.md", "# Note\n\n---\nowner: Alice\n---\nAlice owns the launch.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	complete := func(string) (string, error) {
		calls++
		return `[{"subject":"Alice","relation":"owns","object":"launch","quote":"Alice owns the launch."}]`, nil
	}
	k := New(ix, v)
	k.SetCompleterVersion("model-1", complete)
	got := k.Extract([]string{"note.md"}, false, nil)
	if got[0].Status != "indexed" || calls != 1 {
		t.Fatalf("first extraction=%+v calls=%d", got, calls)
	}
	if _, err = k.Snapshot(nil, GraphOptions{IncludeDocuments: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("graph invoked model: %d calls", calls)
	}
	k2 := New(ix, v)
	k2.SetCompleterVersion("model-1", complete)
	got = k2.Extract([]string{"note.md"}, false, nil)
	if got[0].Status != "cached" || calls != 1 {
		t.Fatalf("restart cache=%+v calls=%d", got, calls)
	}
	if _, err = v.Write("note.md", "# Note\n\nAlice owns the revised launch.", nil); err != nil {
		t.Fatal(err)
	}
	if _, err = ix.Upsert("note.md"); err != nil {
		t.Fatal(err)
	}
	got = k2.Extract([]string{"note.md"}, false, nil)
	if got[0].Status != "indexed" || calls != 2 {
		t.Fatalf("edit cache=%+v calls=%d", got, calls)
	}
	k2.SetCompleterVersion("model-2", complete)
	got = k2.Extract([]string{"note.md"}, false, nil)
	if got[0].Status != "indexed" || calls != 3 {
		t.Fatalf("model change=%+v calls=%d", got, calls)
	}
	if err = ix.Remove("note.md"); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, "note.md")); err != nil {
		t.Fatal(err)
	}
	graph, err := k2.Snapshot(nil, GraphOptions{IncludeDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range graph.Nodes {
		if n.Path == "note.md" {
			t.Fatal("deleted note remained in graph")
		}
	}
}

func TestModelOffUsesCacheWithoutExtractingAndLongJobsAreBounded(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	v, _ := vault.New(root)
	ix := index.New(database, v, embed.Hash{})
	v.Write("note.md", "# Note\n\nAlice owns launch.", nil)
	ix.Reindex()
	calls := 0
	complete := func(string) (string, error) {
		calls++
		return `[{"subject":"Alice","relation":"owns","object":"launch","quote":"Alice owns launch."}]`, nil
	}
	k := New(ix, v)
	k.SetCompleterVersion("m1", complete)
	k.Extract([]string{"note.md"}, false, nil)
	k.SetCompleterVersion("unavailable", nil)
	if _, err := k.Snapshot(nil, GraphOptions{IncludeDocuments: true}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("model called while off: %d", calls)
	}
	large := strings.Repeat("paragraph with no relationship.\n\n", 60000)
	calls = 0
	_, err = extractDocument(large, complete)
	if err == nil || !strings.Contains(err.Error(), "job limit") {
		t.Fatalf("large job error=%v", err)
	}
	if calls != 0 {
		t.Fatalf("oversize job called model: %d", calls)
	}
}

func TestGraphQueryDoesNotCreateGhostNodesOrFoldCasePaths(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	v, _ := vault.New(root)
	ix := index.New(database, v, embed.Hash{})
	for path, body := range map[string]string{"Alpha.md": "# Alpha\n\n[[Beta]]", "Beta.md": "# Beta\n\nbridge", "alpha.md": "# lower\n\nseparate"} {
		if _, err = v.Write(path, body, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	g, err := New(ix, v).Snapshot(nil, GraphOptions{Seed: "Alpha", Q: "Alpha", Depth: 2, IncludeDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if n.ID == "" || n.Label == "" {
			t.Fatalf("ghost node: %+v", n)
		}
	}
	if len(g.Nodes) == 0 {
		t.Fatal("q+seed removed the connected result")
	}
	ids := map[string]bool{}
	g, err = New(ix, v).Snapshot(nil, GraphOptions{IncludeDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range g.Nodes {
		if ids[n.ID] {
			t.Fatalf("folded stable ID: %s", n.ID)
		}
		ids[n.ID] = true
	}
}

func TestSnapshotIsDeterministicWhenRelationHasManySources(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	v, _ := vault.New(root)
	ix := index.New(database, v, embed.Hash{})
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("source-%d.md", i)
		if _, err := v.Write(path, fmt.Sprintf("# Source %d\n\nAlice and Atlas are connected.", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	store := New(ix, v)
	store.triples = map[string]cachedTriple{}
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("source-%d.md", i)
		note, _ := v.Read(path)
		store.triples[path] = cachedTriple{hash: note.Hash, triples: []Triple{{Subject: "Alice", Relation: "connected", Object: "Atlas", Quote: "Alice and Atlas are connected."}}}
	}
	first, err := store.Snapshot(nil, GraphOptions{IncludeDocuments: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		next, err := store.Snapshot(nil, GraphOptions{IncludeDocuments: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("snapshot %d differed", i+1)
		}
	}
}

func TestQueryUsesSemanticEntityBridgeWithoutDocumentMetadata(t *testing.T) {
	root := t.TempDir()
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	v, _ := vault.New(root)
	ix := index.New(database, v, embed.Hash{})
	v.Write("seed.md", "# Seed\n\nalpha launch discussion. Alice connects Atlas.", nil)
	v.Write("bridge.md", "# Bridge\n\nAlice connects Atlas in the research record.", nil)
	if _, err = ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	store := New(ix, v)
	store.SetCompleter(func(string) (string, error) {
		return `[{"subject":"Alice","relation":"connects","object":"Atlas","quote":"Alice connects Atlas"}]`, nil
	})
	for _, result := range store.Extract([]string{"seed.md", "bridge.md"}, false, nil) {
		if result.Status != "indexed" || result.Triples != 1 {
			t.Fatalf("semantic extraction failed: %+v", result)
		}
	}
	result, err := store.Query("alpha", 1, index.Filter{}, nil, "", "", 2, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, citation := range result.Citations {
		if citation.Path == "bridge.md" {
			return
		}
	}
	t.Fatalf("semantic bridge was not cited: %+v", result.Citations)
}
