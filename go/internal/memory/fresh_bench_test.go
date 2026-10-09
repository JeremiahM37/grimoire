package memory

import "testing"

// Recall assesses every fact it returns, so this is the per-fact cost
// freshness adds to a recall on top of ranking.
func BenchmarkAssess(b *testing.B) {
	now := at("2026-10-08 12:00")
	p := DefaultPriors()
	e := Entry{Text: "grimoire on AIServer is build 1.4.0-dev listening on :9111",
		Stamp: "2026-09-01 10:00", Since: "2026-07-01 10:00", Verified: "2026-09-20 10:00",
		Fresh: "7d", Check: "grimoire version", Changes: 3, Verifies: 2}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = e.Assess(now, p, 0)
	}
}
