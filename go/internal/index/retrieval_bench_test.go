package index

import (
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Scale benchmarks for the retrieval hot path.
//
// The corpus is written straight into the index tables rather than through the
// vault, because what is being measured is query cost against a corpus of a
// given size — not how fast notes can be ingested. Vocabulary is Zipf-ish so
// document frequencies look like prose: a few very common terms, a long tail of
// rare ones. A uniform vocabulary would make every BM25 term equally selective
// and hide exactly the skew that matters.

var benchVocab = func() []string {
	words := make([]string, 4000)
	for i := range words {
		words[i] = fmt.Sprintf("term%04d", i)
	}
	return words
}()

// benchWord picks a word with a Zipf-like bias toward the front of the vocabulary.
func benchWord(r *rand.Rand) string {
	i := int(float64(len(benchVocab)) * r.Float64() * r.Float64() * r.Float64())
	if i >= len(benchVocab) {
		i = len(benchVocab) - 1
	}
	return benchVocab[i]
}

// benchIndex builds an index holding nChunks chunks of ~40 words each.
func benchIndex(tb testing.TB, nChunks int) *Index {
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

	r := rand.New(rand.NewSource(42))
	const chunksPerNote = 8
	nNotes := nChunks / chunksPerNote
	if nNotes == 0 {
		nNotes = 1
	}
	for n := 0; n < nNotes; n++ {
		path := fmt.Sprintf("bench/note%06d.md", n)
		title := fmt.Sprintf("Note %d", n)
		if err := ix.DB.Exec(
			"INSERT INTO notes(path,title,body,frontmatter_json,private,mtime,hash,created,updated)"+
				" VALUES(?,?,?,?,0,0,'','','')", path, title, "", "{}"); err != nil {
			tb.Fatal(err)
		}
		for c := 0; c < chunksPerNote; c++ {
			var sb strings.Builder
			for w := 0; w < 40; w++ {
				if w > 0 {
					sb.WriteByte(' ')
				}
				sb.WriteString(benchWord(r))
			}
			chunk := sb.String()
			vec := ix.Emb.Embed([]string{chunk})[0]
			if err := ix.DB.Exec(
				"INSERT INTO vectors(note,chunk_idx,chunk,embedding,private) VALUES(?,?,?,?,0)",
				path, c, chunk, Pack(vec)); err != nil {
				tb.Fatal(err)
			}
		}
	}
	return ix
}

// benchQueries returns deterministic multi-term queries drawn from the vocabulary.
func benchQueries(n int) []string {
	r := rand.New(rand.NewSource(7))
	out := make([]string, n)
	for i := range out {
		terms := make([]string, 6)
		for j := range terms {
			terms[j] = benchWord(r)
		}
		out[i] = strings.Join(terms, " ")
	}
	return out
}

// benchmarkRetrieve measures STEADY-STATE query cost: the corpus cache is
// warmed before the timer starts.
//
// Warming is not flattering the number, it is isolating one. Without it a
// low -benchtime silently divides the one-off cache build across b.N and
// reports the average as if it were per-query cost — which is how an early
// version of this benchmark appeared to show 45 MB allocated per search.
// Build cost is real, but it is a different cost with a different trigger
// (an index write), so it gets its own benchmark below.
func benchmarkRetrieve(b *testing.B, nChunks int) {
	ix := benchIndex(b, nChunks)
	queries := benchQueries(64)
	if _, err := ix.Retrieve(queries[0], 8, false); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.Retrieve(queries[i%len(queries)], 8, false); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRetrieve1k(b *testing.B)   { benchmarkRetrieve(b, 1_000) }
func BenchmarkRetrieve10k(b *testing.B)  { benchmarkRetrieve(b, 10_000) }
func BenchmarkRetrieve50k(b *testing.B)  { benchmarkRetrieve(b, 50_000) }
func BenchmarkRetrieve200k(b *testing.B) { benchmarkRetrieve(b, 200_000) }

// benchmarkCacheBuild measures the rebuild an index write forces on the next
// search. This is the cost that decides whether a write-heavy vault should
// keep invalidating the whole cache or move to incremental maintenance.
func benchmarkCacheBuild(b *testing.B, nChunks int) {
	ix := benchIndex(b, nChunks)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ix.InvalidateCache()
		if _, err := ix.corpusCacheFor(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkCacheBuild1k(b *testing.B)   { benchmarkCacheBuild(b, 1_000) }
func BenchmarkCacheBuild10k(b *testing.B)  { benchmarkCacheBuild(b, 10_000) }
func BenchmarkCacheBuild50k(b *testing.B)  { benchmarkCacheBuild(b, 50_000) }
func BenchmarkCacheBuild200k(b *testing.B) { benchmarkCacheBuild(b, 200_000) }

// The MEMORY ranking path at size.
//
// The note-retrieval benchmarks above never touched it, which is how ranking
// came to re-extract every candidate's entities from its text on every query —
// the same deterministic work indexing had already done and stored in
// memory_entities, a table that was written on every write and never read.
// Nothing measured that path, so nothing objected.
func benchmarkMemoryRank(b *testing.B, nEntries int) {
	root := b.TempDir()
	v, err := vault.New(root)
	if err != nil {
		b.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { database.Close() })
	ix := New(database, v, embed.Hash{})

	// Entries spread over notes the way a real vault holds them, with the
	// proper nouns and identifiers the entity signal exists to match.
	const perNote = 50
	hosts := []string{"AIServer", "MediaServer", "db-01.prod", "Grafana", "PgBouncer"}
	people := []string{"Priya Sharma", "Dana Ruiz", "Sam Okafor"}
	for i := 0; i < nEntries; i += perNote {
		var body strings.Builder
		body.WriteString("# Memory\n\n")
		for j := 0; j < perNote && i+j < nEntries; j++ {
			n := i + j
			fmt.Fprintf(&body, "- **2026-08-14 09:%02d · agent** — %s restarted %s on %s "+
				"after the deploy, port %d <!--m id=e%d-->\n",
				n%60, people[n%len(people)], hosts[n%len(hosts)], hosts[(n+1)%len(hosts)],
				5000+n%900, n)
		}
		rel := fmt.Sprintf("memory/note-%05d.md", i/perNote)
		if _, err := v.Write(rel, body.String(), nil); err != nil {
			b.Fatal(err)
		}
		if _, err := ix.Upsert(rel); err != nil {
			b.Fatal(err)
		}
	}

	queries := []string{
		"who restarted Grafana on AIServer",
		"what port does PgBouncer use on db-01.prod",
		"Priya Sharma deploy MediaServer",
	}
	if _, err := ix.MemoryEntries(MemoryQuery{Query: queries[0], Limit: 10}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ix.MemoryEntries(MemoryQuery{
			Query: queries[i%len(queries)], Limit: 10,
		}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMemoryRank1k(b *testing.B)  { benchmarkMemoryRank(b, 1_000) }
func BenchmarkMemoryRank10k(b *testing.B) { benchmarkMemoryRank(b, 10_000) }
func BenchmarkMemoryRank50k(b *testing.B) { benchmarkMemoryRank(b, 50_000) }
