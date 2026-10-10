package index

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// BenchmarkRecallExpand measures the recall cost of the expansion and graph
// stages against the plain recall they extend. The same corpus and the same
// queries are used for every arm, so the difference is the stages themselves.
//
// The corpus has the entity shape the walk exists for: a few people and hosts
// that many facts share, a first name that resolves to one full name, and
// stems the keyword variant has to reach.
func BenchmarkRecallExpand(b *testing.B) {
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

	const nEntries, perNote = 1000, 50
	hosts := []string{"AIServer", "MediaServer", "db-01.prod", "Grafana", "PgBouncer"}
	people := []string{"Priya Sharma", "Dana Kim", "Sam Okafor"}
	actions := []string{"restarted", "deployed", "rotated", "reviewed"}
	for i := 0; i < nEntries; i += perNote {
		var body strings.Builder
		body.WriteString("# Memory\n\n")
		for j := 0; j < perNote && i+j < nEntries; j++ {
			n := i + j
			fmt.Fprintf(&body, "- **2026-08-14 09:%02d · agent** — %s %s %s on %s "+
				"after the deploy, port %d <!--m id=e%d-->\n",
				n%60, people[n%len(people)], actions[n%len(actions)],
				hosts[n%len(hosts)], hosts[(n+1)%len(hosts)], 5000+n%900, n)
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
		"Dana deployed PgBouncer",
		"what port does db-01.prod use",
		"Priya reviewed MediaServer",
	}
	arms := []struct {
		name string
		opt  ExpandOptions
	}{
		{"off", ExpandOptions{}},
		{"expand", ExpandOptions{Expand: true}},
		{"expand_hops2", ExpandOptions{Expand: true, Hops: 2}},
	}
	for _, arm := range arms {
		b.Run(arm.name, func(b *testing.B) {
			if _, err := ix.RecallExpanded(MemoryQuery{Query: queries[0], Limit: 10}, arm.opt); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ix.RecallExpanded(MemoryQuery{
					Query: queries[i%len(queries)], Limit: 10,
				}, arm.opt); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
