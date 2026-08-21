package readlog

import (
	"fmt"
	"testing"
	"time"
)

// Reading the trail back. Every test here pins Event.At, because the whole
// feature is about time windows and a test that cannot place events in time
// can only assert that nothing happens.

var base = time.Date(2026, 8, 21, 10, 0, 0, 0, time.UTC)

// reads records n allowed reads of distinct documents, one every `gap`.
func reads(l *Log, user string, n int, start time.Time, gap time.Duration) {
	for i := 0; i < n; i++ {
		l.Record(Event{
			At: start.Add(time.Duration(i) * gap), User: user, Name: user,
			Path: fmt.Sprintf("hr/doc-%02d.md", i), Space: "hr", Allowed: true,
			Route: "GET /api/notes",
		})
	}
}

func scan(t *testing.T, l *Log, opt Options) []Anomaly {
	t.Helper()
	l.Flush()
	if opt.Now.IsZero() {
		opt.Now = base.Add(time.Hour)
	}
	if opt.Since.IsZero() {
		opt.Since = base.Add(-time.Hour)
	}
	out, err := l.Anomalies(opt)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestOrdinaryWorkIsNotAnAnomaly(t *testing.T) {
	// The property that decides whether anybody keeps this switched on: a
	// person opening a handful of documents over an afternoon must produce
	// nothing at all.
	l := New(open(t))
	l.Start()
	defer l.Close()
	reads(l, "u1", 6, base, 3*time.Minute)

	if found := scan(t, l, Options{}); len(found) != 0 {
		t.Errorf("ordinary reading tripped the detector: %+v", found)
	}
}

func TestASweepIsCaught(t *testing.T) {
	l := New(open(t))
	l.Start()
	defer l.Close()
	reads(l, "u1", 40, base, 2*time.Second)

	found := scan(t, l, Options{})
	if len(found) == 0 {
		t.Fatal("a 40-document sweep in 80 seconds produced no anomaly")
	}
	a := found[0]
	if a.Kind != "breadth" || a.User != "u1" {
		t.Fatalf("anomaly = %+v", a)
	}
	if a.Count < 40 {
		t.Errorf("count = %d, want the distinct documents", a.Count)
	}
	// A sample, not the whole list: an operator deciding whether to care does
	// not need forty paths, and echoing every restricted document a caller
	// touched would be its own disclosure.
	if len(a.Documents) == 0 || len(a.Documents) > maxSampleDocs {
		t.Errorf("documents = %d, want a capped sample", len(a.Documents))
	}
}

func TestASweepStraddlingAWindowBoundaryIsStillCaught(t *testing.T) {
	// The failure mode of fixed buckets, and the reason this uses a sliding
	// window: with hourly (or five-minutely) buckets, a sweep that starts near
	// the boundary is two half-sweeps and neither trips the threshold — which
	// is trivially exploitable by anyone who has noticed the boundary.
	l := New(open(t))
	l.Start()
	defer l.Close()
	// Straddles 10:05:00 exactly: half before, half after.
	reads(l, "u1", 30, base.Add(4*time.Minute+30*time.Second), 2*time.Second)

	found := scan(t, l, Options{})
	if len(found) == 0 {
		t.Fatal("a sweep across the window boundary produced no anomaly")
	}
	if found[0].Count < 30 {
		t.Errorf("count = %d — the burst was split across buckets", found[0].Count)
	}
}

func TestTheSameDocumentReadRepeatedlyIsNotBreadth(t *testing.T) {
	// Somebody refreshing one page all morning is not exfiltrating anything.
	// Counting attempts rather than DISTINCT documents would flag them and
	// nobody else.
	l := New(open(t))
	l.Start()
	defer l.Close()
	for i := 0; i < 60; i++ {
		l.Record(Event{At: base.Add(time.Duration(i) * time.Second), User: "u1",
			Name: "u1", Path: "hr/one.md", Space: "hr", Allowed: true})
	}
	if found := scan(t, l, Options{}); len(found) != 0 {
		t.Errorf("re-reading one document tripped the breadth detector: %+v", found)
	}
}

func TestARunOfDenialsIsCaught(t *testing.T) {
	// The more interesting half: somebody walking paths they cannot open looks
	// like nothing at all in a model that only logs successes.
	l := New(open(t))
	l.Start()
	defer l.Close()
	for i := 0; i < 9; i++ {
		l.Record(Event{At: base.Add(time.Duration(i) * time.Second), User: "u2",
			Name: "mallory", Path: fmt.Sprintf("hr/secret-%d.md", i),
			Space: "hr", Allowed: false})
	}

	found := scan(t, l, Options{})
	if len(found) == 0 {
		t.Fatal("nine refusals in nine seconds produced no anomaly")
	}
	if found[0].Kind != "denials" || found[0].Name != "mallory" {
		t.Fatalf("anomaly = %+v", found[0])
	}
}

func TestDeniedReadsDoNotInflateBreadth(t *testing.T) {
	// A caller must not be able to trip the breadth threshold with documents
	// it never actually received — that would make the two signals report the
	// same event twice and mean different things.
	l := New(open(t))
	l.Start()
	defer l.Close()
	for i := 0; i < 40; i++ {
		l.Record(Event{At: base.Add(time.Duration(i) * time.Second), User: "u2",
			Name: "mallory", Path: fmt.Sprintf("hr/secret-%d.md", i),
			Space: "hr", Allowed: false})
	}
	found := scan(t, l, Options{})
	for _, a := range found {
		if a.Kind == "breadth" {
			t.Errorf("denials were counted as breadth: %+v", a)
		}
	}
}

func TestCallersAreNotPooled(t *testing.T) {
	// Ten people each reading five documents is a normal morning, not a
	// thirty-document sweep. Pooling them would make the detector fire on busy
	// instances and never on quiet compromised ones.
	l := New(open(t))
	l.Start()
	defer l.Close()
	for u := 0; u < 10; u++ {
		reads(l, fmt.Sprintf("u%d", u), 5, base.Add(time.Duration(u)*time.Second), time.Second)
	}
	if found := scan(t, l, Options{}); len(found) != 0 {
		t.Errorf("ten normal readers pooled into an anomaly: %+v", found)
	}
}

func TestUnauthenticatedCallersAreSeparatedByAddress(t *testing.T) {
	// Every anonymous caller has the same (empty) account. Grouping them
	// together would hide exactly the scan worth seeing.
	l := New(open(t))
	l.Start()
	defer l.Close()
	for i := 0; i < 40; i++ {
		l.Record(Event{At: base.Add(time.Duration(i) * time.Second),
			Path: fmt.Sprintf("hr/doc-%d.md", i), Space: "hr",
			Allowed: true, Addr: "10.0.0.9"})
	}
	for i := 0; i < 3; i++ {
		l.Record(Event{At: base.Add(time.Duration(i) * time.Second),
			Path: fmt.Sprintf("hr/other-%d.md", i), Space: "hr",
			Allowed: true, Addr: "10.0.0.10"})
	}
	found := scan(t, l, Options{})
	if len(found) != 1 {
		t.Fatalf("want exactly the sweeping address, got %+v", found)
	}
	if found[0].Addr != "10.0.0.9" {
		t.Errorf("anomaly addr = %q", found[0].Addr)
	}
}

func TestThresholdsAreAdjustable(t *testing.T) {
	l := New(open(t))
	l.Start()
	defer l.Close()
	reads(l, "u1", 8, base, time.Second)

	if found := scan(t, l, Options{}); len(found) != 0 {
		t.Fatalf("eight reads tripped the default threshold: %+v", found)
	}
	if found := scan(t, l, Options{Breadth: 5}); len(found) != 1 {
		t.Errorf("breadth=5 found %d anomalies in eight reads", len(found))
	}
}

func TestAnOldSweepFallsOutOfTheHorizon(t *testing.T) {
	l := New(open(t))
	l.Start()
	defer l.Close()
	reads(l, "u1", 40, base.Add(-48*time.Hour), time.Second)

	// The default horizon is a day.
	found, err := l.Anomalies(Options{Now: base})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Errorf("a two-day-old sweep is still reported: %+v", found)
	}
	// …but it is still there if you ask for it.
	found = scan(t, l, Options{Since: base.Add(-72 * time.Hour)})
	if len(found) == 0 {
		t.Error("an explicit since could not reach the old sweep")
	}
}

func TestAnEmptyTrailIsNotAnAllClear(t *testing.T) {
	// On a single-user instance nothing is restricted, so nothing is recorded.
	// A surface that cannot tell "no anomalies" from "no data" reassures
	// people about a check that never ran.
	l := New(open(t))
	l.Start()
	defer l.Close()
	if n := l.Count(); n != 0 {
		t.Fatalf("fresh trail holds %d records", n)
	}
	reads(l, "u1", 2, base, time.Minute)
	l.Flush()
	if n := l.Count(); n != 2 {
		t.Errorf("Count = %d after two reads", n)
	}
}

func TestScanningIsBoundedByOneUser(t *testing.T) {
	l := New(open(t))
	l.Start()
	defer l.Close()
	reads(l, "u1", 40, base, time.Second)
	reads(l, "u2", 40, base, time.Second)

	found := scan(t, l, Options{OnlyUser: "u1"})
	for _, a := range found {
		if a.User != "u1" {
			t.Errorf("user=u1 returned an anomaly for %q", a.User)
		}
	}
	if len(found) == 0 {
		t.Error("filtering by user returned nothing at all")
	}
}
