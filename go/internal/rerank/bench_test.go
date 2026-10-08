package rerank

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// benchDocs returns n distinct documents of roughly words words each.
func benchDocs(n, words int) []string {
	vocab := strings.Fields("the backup job verifies restic snapshots nightly while the thin " +
		"pool on the cluster node fills with guest disks and the operator moves bulk data " +
		"to a separate volume so root keeps free space for logs caches and packages")
	docs := make([]string, n)
	for i := range docs {
		var b strings.Builder
		for w := 0; w < words; w++ {
			b.WriteString(vocab[(i*7+w*3)%len(vocab)])
			b.WriteByte(' ')
		}
		docs[i] = b.String()
	}
	return docs
}

// BenchmarkScore scores n pairs truncated to exactly seq tokens each
// (documents are longer than any seq), plus the "typical" case of ~60-token
// documents with no truncation.
func BenchmarkScore(b *testing.B) {
	dir := testModelDir(b)
	const query = "how are the cluster backups verified"
	run := func(name string, docs []string, maxLen int) {
		b.Run(name, func(b *testing.B) {
			l, err := LoadLocal(dir, LocalConfig{MaxLen: maxLen})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := l.Score(context.Background(), query, docs[:1]); err != nil {
				b.Fatal(err)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := l.Score(context.Background(), query, docs); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/call")
			b.ReportMetric(float64(len(docs)*b.N)/b.Elapsed().Seconds(), "pairs/s")
		})
	}
	run("typical/pairs=100/doc~60tok", benchDocs(100, 52), DefaultMaxLen)
	for _, seq := range []int{64, 128, 256} {
		for _, n := range []int{50, 100, 300} {
			run(fmt.Sprintf("seq=%d/pairs=%d", seq, n), benchDocs(n, 400), seq)
		}
	}
}
