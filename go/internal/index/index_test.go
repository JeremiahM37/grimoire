package index

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

func testIndex(t *testing.T) *Index {
	t.Helper()
	root := t.TempDir()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database, v, embed.Hash{})
}

func write(t *testing.T, ix *Index, rel, body string) {
	t.Helper()
	if _, err := ix.Vault.Write(rel, body, nil); err != nil {
		t.Fatal(err)
	}
}

func TestReindexPopulatesEverything(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "a.md", "# Alpha\n\nbody about gateways #infra\n\nport:: 8443\n")
	write(t, ix, "sub/b.md", "# Beta\n\nlinks to [[Alpha]] and [[sub/b]]\n")

	n, err := ix.Reindex()
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("indexed %d notes, want 2", n)
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM notes"); c != 2 {
		t.Errorf("notes rows = %d", c)
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM tags WHERE tag='infra'"); c != 1 {
		t.Errorf("tag not indexed")
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM facts WHERE key='port' AND value='8443'"); c != 1 {
		t.Errorf("fact not indexed")
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM vectors"); c == 0 {
		t.Errorf("no vectors written")
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM links WHERE resolved=0"); c != 0 {
		t.Errorf("%d links unresolved; folder-qualified targets should resolve", c)
	}
}

// The regression that motivated the equivalent Python fix: [[Folder/Note]] is
// how every editor links into a subfolder.
func TestFolderQualifiedLinksResolve(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "Job Search/Strategy.md", "# Strategy\n\nthe plan\n")
	write(t, ix, "index.md", "see [[Job Search/Strategy]] and [[Job Search/Strategy.md]]\n")
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	rows, err := ix.DB.Query("SELECT target, dst, resolved FROM links WHERE src='index.md'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var target, dst string
		var resolved int
		if err := rows.Scan(&target, &dst, &resolved); err != nil {
			t.Fatal(err)
		}
		seen++
		if resolved != 1 || dst != "Job Search/Strategy.md" {
			t.Errorf("%q resolved=%d dst=%q", target, resolved, dst)
		}
	}
	if seen == 0 {
		t.Error("no links indexed")
	}
}

func TestUpsertAndRemove(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "a.md", "# A\n\nalpha\n")
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	write(t, ix, "b.md", "# B\n\nbeta\n")
	if _, err := ix.Upsert("b.md"); err != nil {
		t.Fatal(err)
	}
	if c, _ := ix.DB.Count("SELECT COUNT(*) FROM notes"); c != 2 {
		t.Fatalf("after upsert notes = %d, want 2", c)
	}
	if err := ix.Remove("b.md"); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"notes", "vectors", "tags", "facts"} {
		c, _ := ix.DB.Count("SELECT COUNT(*) FROM " + tbl + " WHERE " +
			map[string]string{"notes": "path", "vectors": "note",
				"tags": "note", "facts": "note"}[tbl] + "='b.md'")
		if c != 0 {
			t.Errorf("%s still has rows for the removed note", tbl)
		}
	}
}

// Upserting the same note repeatedly must not accumulate duplicate rows — the
// delete-then-insert has to cover every table.
func TestUpsertIsIdempotent(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "a.md", "# A\n\nalpha #tag\n\nkey:: value\n")
	for i := 0; i < 5; i++ {
		if _, err := ix.Upsert("a.md"); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		"SELECT COUNT(*) FROM notes",
		"SELECT COUNT(*) FROM tags",
		"SELECT COUNT(*) FROM facts",
	} {
		if c, _ := ix.DB.Count(q); c != 1 {
			t.Errorf("%s = %d, want 1 (duplicate rows accumulated)", q, c)
		}
	}
}

func TestRetrieveRanksRelevantChunk(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "ports.md", "# Ports\n\nthe api gateway listens on port 8443\n")
	write(t, ix, "owner.md", "# Owner\n\nthe deploy service is owned by the platform team\n")
	write(t, ix, "noise.md", "# Noise\n\ncompletely unrelated content about gardening\n")
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Retrieve("what port does the api gateway listen on", 3, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("no hits")
	}
	if hits[0].Path != "ports.md" {
		t.Errorf("top hit = %s, want ports.md (hits: %+v)", hits[0].Path, hits)
	}
	if !strings.Contains(hits[0].Chunk, "8443") {
		t.Errorf("top chunk missing the answer: %q", hits[0].Chunk)
	}
}

// Private notes must stay out of retrieval unless explicitly opted in — this
// feeds surfaces that are not necessarily authenticated.
func TestRetrieveExcludesPrivateByDefault(t *testing.T) {
	ix := testIndex(t)
	// set private through frontmatter, not by embedding a block in the body:
	// Write() stamps its own frontmatter, so a leading --- in the body text
	// would just become content.
	fm := markdown.NewFrontmatter()
	fm.Set("private", true)
	if _, err := ix.Vault.Write("secret.md", "# Secret\n\nthe launch code is hunter2\n", fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	hits, err := ix.Retrieve("launch code", 5, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.Path == "secret.md" {
			t.Fatalf("private note leaked into default retrieval: %+v", h)
		}
	}
	hits, err = ix.Retrieve("launch code", 5, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.Path == "secret.md" {
			found = true
		}
	}
	if !found {
		t.Error("private note not returned even when opted in")
	}
}

func TestExtractFactsSkipsFencedCode(t *testing.T) {
	facts := ExtractFacts("port:: 8443\n\n```\nfake:: not a fact\n```\n\n- role:: primary\n")
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2: %+v", len(facts), facts)
	}
	if facts[0].Key != "port" || facts[0].Value != "8443" {
		t.Errorf("facts[0] = %+v", facts[0])
	}
	if facts[1].Key != "role" || facts[1].Value != "primary" {
		t.Errorf("facts[1] = %+v", facts[1])
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	v := []float32{0, 1, -1, 0.5, 1e-8}
	got := Unpack(Pack(v))
	if len(got) != len(v) {
		t.Fatalf("length %d, want %d", len(got), len(v))
	}
	for i := range v {
		if got[i] != v[i] {
			t.Errorf("component %d: %v != %v", i, got[i], v[i])
		}
	}
}

// Re-indexing a note must replace its full-text row, not add another.
//
// The row is deleted through fts_map because fts.path is UNINDEXED, and a map
// that does not match leaves the old row in place: search then answers from
// both the old and new text, which for an encrypted note means answering with
// the plaintext it was just sealed to hide.
func TestFTSRowIsReplacedNotDuplicated(t *testing.T) {
	ix := testIndex(t)
	write(t, ix, "note.md", "# Note\n\noriginal body zarquon")
	if _, err := ix.Upsert("note.md"); err != nil {
		t.Fatal(err)
	}
	write(t, ix, "note.md", "# Note\n\nrewritten body")
	if _, err := ix.Upsert("note.md"); err != nil {
		t.Fatal(err)
	}

	rows, err := ix.DB.Count("SELECT COUNT(*) FROM fts WHERE path=?", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("%d full-text rows for one note, want 1", rows)
	}
	stale, err := ix.DB.Count("SELECT COUNT(*) FROM fts WHERE body LIKE '%zarquon%'")
	if err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Fatal("the superseded text is still searchable")
	}
	mapped, err := ix.DB.Count("SELECT COUNT(*) FROM fts_map WHERE path=?", "note.md")
	if err != nil {
		t.Fatal(err)
	}
	if mapped != 1 {
		t.Fatalf("%d map entries for one note, want 1", mapped)
	}

	// and removing the note takes both with it
	if err := ix.Remove("note.md"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"SELECT COUNT(*) FROM fts", "SELECT COUNT(*) FROM fts_map"} {
		n, err := ix.DB.Count(q)
		if err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%s = %d after removing the only note", q, n)
		}
	}
}
