package index

import (
	"math"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// actrDay is a time n days after fixedNow, with fractional days allowed.
func actrDay(n float64) time.Time {
	return fixedNow.Add(time.Duration(n * float64(24*time.Hour)))
}

func TestActrBaseMatchesDirectSum(t *testing.T) {
	now := actrDay(0)
	tr := actrTrace{
		Creation: actrDay(-100),
		Events:   []time.Time{actrDay(-10), actrDay(-2), actrDay(-0.5)},
	}
	want := math.Log(math.Pow(100, -0.5) + math.Pow(10, -0.5) + math.Pow(2, -0.5) + math.Pow(0.5, -0.5))
	got, ok := actrBase(tr, now)
	if !ok || math.Abs(got-want) > 1e-9 {
		t.Fatalf("B = %v (ok %v), want %v", got, ok, want)
	}
}

func TestActrFutureEventsAreIgnoredNotClamped(t *testing.T) {
	now := actrDay(0)
	base, _ := actrBase(actrTrace{Creation: actrDay(-5)}, now)
	with, _ := actrBase(actrTrace{Creation: actrDay(-5), Events: []time.Time{actrDay(3)}}, now)
	if base != with {
		t.Fatalf("a future event moved B: %v -> %v", base, with)
	}
}

func TestActrNoHistoryIsNotUsable(t *testing.T) {
	if _, ok := actrBase(actrTrace{}, actrDay(0)); ok {
		t.Fatal("an empty trace must not produce an activation")
	}
}

// A fact used often across a year beats a fact used three times last week:
// frequency over a long span can outweigh a short burst, which the fixed
// recency term cannot express.
func TestActrRanksSteadyUseAboveRecentBurst(t *testing.T) {
	now := actrDay(0)
	var steady []time.Time
	for i := 0; i < 50; i++ {
		steady = append(steady, actrDay(-365+float64(i)*365/50))
	}
	burst := []time.Time{actrDay(-1), actrDay(-2), actrDay(-3)}
	a, _ := actrBase(actrTrace{Creation: actrDay(-365), Events: steady}, now)
	b, _ := actrBase(actrTrace{Creation: actrDay(-365), Events: burst}, now)
	if !(a > b) {
		t.Fatalf("steady B = %.3f, burst B = %.3f; want steady higher", a, b)
	}
}

// Folding the oldest events into a summary must stay close to the exact sum,
// or the bounded log quietly changes rankings.
func TestActrFoldingApproximatesExactSum(t *testing.T) {
	now := actrDay(0)
	var all []time.Time
	for i := 0; i < 500; i++ {
		all = append(all, actrDay(-730+float64(i)*730/500))
	}
	exact, _ := actrBase(actrTrace{Creation: actrDay(-800), Events: all}, now)

	kept := all[len(all)-actrKeep:]
	folded := all[:len(all)-actrKeep]
	approx, _ := actrBase(actrTrace{
		Creation:  actrDay(-800),
		Events:    kept,
		Folded:    len(folded),
		FoldFirst: folded[0],
		FoldLast:  folded[len(folded)-1],
	}, now)
	if math.Abs(exact-approx) > 0.05 {
		t.Fatalf("folded B = %.4f, exact B = %.4f: error %.4f exceeds 0.05", approx, exact, exact-approx)
	}
}

func TestActrFlagOffTouchesNothing(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md", entry("used", daysAgo(3), "the deploy host is prod-1"))
	if err := ix.RecordUse("memory/ops.md", "used", fixedNow); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := ix.DB.QueryRow("SELECT count(*) FROM sqlite_master WHERE name='actr_events'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("the flag-off path created the activation table")
	}
	if ix.actrTraces(nil) != nil {
		t.Fatal("actrTraces must return nil with the flag off")
	}
}

// With the flag on, a use is logged with its time, the log stays bounded, and
// the folded summary keeps the count. The stored-and-folded B must match the
// B of the full history to the same tolerance as the pure-math test.
func TestActrRecordBoundsTheLogAndKeepsTheCount(t *testing.T) {
	restore := exp.Set(ActrFlag)
	defer restore()
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md", entry("used", daysAgo(400), "the deploy host is prod-1"))
	var all []time.Time
	for i := 0; i < 100; i++ {
		at := actrDay(-300 + float64(i)*3)
		all = append(all, at)
		if err := ix.actrRecord("memory/ops.md", "used", at); err != nil {
			t.Fatal(err)
		}
	}
	var stored, folded int
	if err := ix.DB.QueryRow("SELECT count(*) FROM actr_events").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if err := ix.DB.QueryRow("SELECT n FROM actr_folded WHERE id='used'").Scan(&folded); err != nil {
		t.Fatal(err)
	}
	if stored != actrKeep || folded != 100-actrKeep {
		t.Fatalf("stored %d, folded %d; want %d and %d", stored, folded, actrKeep, 100-actrKeep)
	}
	traces := ix.actrTraces([]memoryRow{{hit: MemoryHit{Note: "memory/ops.md", Entry: memory.Entry{ID: "used", Stamp: daysAgo(400)}}}})
	tr := traces[entKey{"memory/ops.md", "used"}]
	got, ok := actrBase(tr, actrDay(0))
	exact, _ := actrBase(actrTrace{Creation: actrDay(-400), Events: all}, actrDay(0))
	if !ok || math.Abs(got-exact) > 0.05 {
		t.Fatalf("stored B = %.4f, exact = %.4f", got, exact)
	}
}

// Flag on: a fact used recently outranks an identical fact with no uses. Flag
// off: the same pair keeps the fixed-half-life order.
func TestActrRankingUsesTheTraceOnlyUnderTheFlag(t *testing.T) {
	ix := testIndex(t)
	memNote(t, ix, "memory/ops.md",
		entry("used", daysAgo(60), "the deploy host is prod-1"),
		entry("idle", daysAgo(60), "the deploy host is prod-2"))
	restoreOn := exp.Set(ActrFlag) // events are only written under the flag
	for i := 0; i < 3; i++ {
		if err := ix.RecordUse("memory/ops.md", "used", fixedNow.Add(-time.Duration(i)*time.Hour)); err != nil {
			restoreOn()
			t.Fatal(err)
		}
	}
	restoreOn()
	q := MemoryQuery{Query: "deploy host", Now: fixedNow, Limit: 10, LexicalOnly: true}

	restore := exp.Set()
	off, err := ix.MemoryEntries(q)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	restore = exp.Set(ActrFlag)
	on, err := ix.MemoryEntries(q)
	restore()
	if err != nil {
		t.Fatal(err)
	}
	usedOn, _ := hitByID(on, "used")
	idleOn, _ := hitByID(on, "idle")
	if !(usedOn.Recency > idleOn.Recency) {
		t.Fatalf("flag on: used recency %.4f, idle %.4f; want used higher", usedOn.Recency, idleOn.Recency)
	}
	usedOff, _ := hitByID(off, "used")
	idleOff, _ := hitByID(off, "idle")
	if usedOff.Recency != idleOff.Recency {
		t.Fatalf("flag off: recency differs (%.4f vs %.4f) although the stamps match", usedOff.Recency, idleOff.Recency)
	}
}
