package index

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Retrieval is the one part of this codebase whose OUTPUT is a published
// claim: the LoCoMo and LongMemEval numbers were measured on this exact
// ranking, and the property that licenses quoting them is that a rebuild
// reproduces the same contexts byte for byte. Any optimization here therefore
// has to be provably output-preserving, not just "close enough".
//
// So the hot path is pinned by a golden digest rather than by spot assertions.
// The corpus deliberately includes the cases that make a ranking fragile:
// private rows (which change the corpus statistics, not just the output rows),
// multi-chunk notes (which exercise per-note dedup and neighbour merging), and
// exact score ties (which are resolved by corpus order alone).
//
// Regenerate deliberately, never reflexively: GRIMOIRE_UPDATE_GOLDEN=1 go test
// ./internal/index -run Parity. A diff here means retrieval changed for every
// downstream user, so it wants a reason in the commit message.

// parityCorpus builds a fixed corpus. Held separate from the benchmark corpus
// because this one must never change: its shape is baked into the golden file.
func parityCorpus(tb testing.TB) *Index {
	tb.Helper()
	root := tb.TempDir()
	v, err := vault.New(root)
	if err != nil {
		tb.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { database.Close() })
	ix := New(database, v, embed.Hash{})

	vocab := []string{
		"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta",
		"kappa", "lambda", "mu", "nu", "xi", "omicron", "pi", "rho",
	}
	r := rand.New(rand.NewSource(1337))
	for n := 0; n < 60; n++ {
		path := fmt.Sprintf("parity/note%02d.md", n)
		title := fmt.Sprintf("Parity Note %d %s", n, vocab[n%len(vocab)])
		private := 0
		if n%7 == 0 { // a private slice that also shifts df/avglen
			private = 1
		}
		if err := ix.DB.Exec(
			"INSERT INTO notes(path,title,body,frontmatter_json,private,mtime,hash,created,updated)"+
				" VALUES(?,?,?,?,?,0,'','','')", path, title, "", "{}", private); err != nil {
			tb.Fatal(err)
		}
		nChunks := 1 + n%4 // multi-chunk notes exercise dedup + neighbour merge
		for c := 0; c < nChunks; c++ {
			var sb strings.Builder
			for w := 0; w < 12; w++ {
				if w > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(vocab[r.Intn(len(vocab))])
			}
			chunk := sb.String()
			vec := ix.Emb.Embed([]string{chunk})[0]
			if err := ix.DB.Exec(
				"INSERT INTO vectors(note,chunk_idx,chunk,embedding,private) VALUES(?,?,?,?,?)",
				path, c, chunk, Pack(vec), private); err != nil {
				tb.Fatal(err)
			}
		}
	}
	// Exact ties: identical text in several notes, so the only thing deciding
	// their order is the corpus scan order. This is the case an "equivalent"
	// rewrite is most likely to silently permute.
	for n := 0; n < 4; n++ {
		path := fmt.Sprintf("parity/tie%02d.md", n)
		chunk := "alpha beta gamma delta"
		if err := ix.DB.Exec(
			"INSERT INTO notes(path,title,body,frontmatter_json,private,mtime,hash,created,updated)"+
				" VALUES(?,?,?,?,0,0,'','','')", path, "Tie", "", "{}"); err != nil {
			tb.Fatal(err)
		}
		vec := ix.Emb.Embed([]string{chunk})[0]
		if err := ix.DB.Exec(
			"INSERT INTO vectors(note,chunk_idx,chunk,embedding,private) VALUES(?,?,?,?,0)",
			path, 0, chunk, Pack(vec)); err != nil {
			tb.Fatal(err)
		}
	}
	return ix
}

var parityQueries = []string{
	"alpha",
	"alpha beta",
	"gamma delta epsilon",
	"theta kappa lambda mu nu",
	"omicron",
	"pi rho alpha",
	"nothing matches this query at all",
	"Parity Note 3",
	"beta beta beta",
	"zeta eta",
}

// parityDigest serializes every field retrieval promises a caller, for every
// query, at several k values and both privacy modes.
func parityDigest(tb testing.TB, ix *Index) string {
	tb.Helper()
	h := sha256.New()
	for _, includePrivate := range []bool{false, true} {
		for _, k := range []int{1, 3, 8, 20} {
			for _, q := range parityQueries {
				hits, err := ix.Retrieve(q, k, includePrivate)
				if err != nil {
					tb.Fatal(err)
				}
				fmt.Fprintf(h, "q=%q k=%d priv=%v n=%d\n", q, k, includePrivate, len(hits))
				for i, hit := range hits {
					// Score at full precision: rounding here would hide exactly
					// the drift this test exists to catch.
					fmt.Fprintf(h, "  %d path=%q title=%q score=%.17g chunk=%q\n",
						i, hit.Path, hit.Title, hit.Score, hit.Chunk)
				}
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

const parityGoldenFile = "testdata/retrieval_golden.txt"

func TestRetrieveParity(t *testing.T) {
	ix := parityCorpus(t)
	got := parityDigest(t, ix)

	if os.Getenv("GRIMOIRE_UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(parityGoldenFile, []byte(got+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("golden updated: %s", got)
		return
	}

	want, err := os.ReadFile(parityGoldenFile)
	if err != nil {
		t.Fatalf("reading golden (regenerate with GRIMOIRE_UPDATE_GOLDEN=1): %v", err)
	}
	if strings.TrimSpace(string(want)) != got {
		t.Fatalf("retrieval output changed\n golden: %s\n    got: %s\n"+
			"Retrieval results are a published claim; if this change is intended, "+
			"regenerate with GRIMOIRE_UPDATE_GOLDEN=1 and say why in the commit.",
			strings.TrimSpace(string(want)), got)
	}
}

// TestRetrieveDeterministic guards the property the golden file cannot: that
// repeated calls on the same index agree. A cache that went stale, or an
// iteration over a Go map that leaked into the ranking, shows up here.
func TestRetrieveDeterministic(t *testing.T) {
	ix := parityCorpus(t)
	first := parityDigest(t, ix)
	for i := 0; i < 3; i++ {
		if got := parityDigest(t, ix); got != first {
			t.Fatalf("retrieval is not deterministic across calls: %s != %s", got, first)
		}
	}
}
