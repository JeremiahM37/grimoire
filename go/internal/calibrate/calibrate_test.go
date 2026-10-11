package calibrate

import (
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// repeat returns n copies of ev.
func repeat(ev Event, n int) []Event {
	out := make([]Event, n)
	for i := range out {
		out[i] = ev
	}
	return out
}

// factsFor builds one fact per event, all written by agent, each carrying
// that single signal.
func factsFor(agent string, evs []Event) []Fact {
	out := make([]Fact, 0, len(evs))
	for i, ev := range evs {
		out = append(out, Fact{Agent: agent, ID: agent + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)), Events: []Event{ev}})
	}
	return out
}

func snapshotOf(fs ...[]Fact) Snapshot {
	var s Snapshot
	for _, f := range fs {
		s.Facts = append(s.Facts, f...)
	}
	return s
}

func TestPriorIsNeutralAndBounded(t *testing.T) {
	p := Update(0, 0)
	if m := p.Mean(); m != 0.5 {
		t.Fatalf("prior mean = %v, want 0.5", m)
	}
	lb := Reliability(Counts{})
	if lb <= 0 || lb >= 0.5 {
		t.Fatalf("unknown agent lower bound = %v, want strictly between 0 and 0.5", lb)
	}
}

func TestChallengedAgentRanksBelowGoodAgent(t *testing.T) {
	good := snapshotOf(factsFor("good", repeat(Confirmed, 20)))
	poor := snapshotOf(factsFor("poor",
		append(repeat(Confirmed, 10), repeat(Challenged, 10)...)))
	// Ten confirmations for each side of the book, and it is not even close.
	g, p := good.AgentReliability("good"), poor.AgentReliability("poor")
	if !(g > p) {
		t.Fatalf("good agent %.3f should rank above challenged agent %.3f", g, p)
	}
	if g-p < 0.2 {
		t.Fatalf("gap %.3f is smaller than expected for 20 vs 10 bad events", g-p)
	}
}

func TestAgentReliabilityCountsOnlyItsOwnFacts(t *testing.T) {
	s := snapshotOf(
		factsFor("alpha", repeat(Challenged, 5)),
		factsFor("beta", repeat(Confirmed, 5)),
	)
	if c := s.Counts("alpha"); c.Bad() != 5 || c.Good() != 0 {
		t.Fatalf("alpha counts = %+v", c)
	}
	if c := s.Counts("beta"); c.Good() != 5 || c.Bad() != 0 {
		t.Fatalf("beta counts = %+v", c)
	}
	if s.Counts("") != (Counts{}) {
		t.Fatal("empty agent must have no counts")
	}
}

func TestOneEventDoesNotFlipClearRanking(t *testing.T) {
	// Fifty confirmations against twenty. One challenge against the leader
	// must not put it behind: the margin is far larger than one event's effect.
	ahead := snapshotOf(factsFor("ahead", append(repeat(Confirmed, 50), repeat(Challenged, 1)...)))
	behind := snapshotOf(factsFor("behind", repeat(Confirmed, 20)))
	a := ahead.AgentReliability("ahead")
	b := behind.AgentReliability("behind")
	if !(a > b) {
		t.Fatalf("one challenge flipped a clear ranking: ahead %.3f vs behind %.3f", a, b)
	}
}

// This is a NEGATIVE result, kept as a test so it cannot be forgotten. Thirty
// confirmations against twenty-five is a near-tie in lower bound (about 0.02
// apart), and a single challenge against the leader is enough to reverse it.
// A learner cannot avoid this: when the margin is smaller than one event's
// effect, one event is evidence about which agent is better. Operators should
// read a reliability difference below roughly 0.05 as "not yet separated".
func TestOneEventCanFlipNearTie(t *testing.T) {
	ahead := snapshotOf(factsFor("ahead", append(repeat(Confirmed, 30), repeat(Challenged, 1)...)))
	behind := snapshotOf(factsFor("behind", repeat(Confirmed, 25)))
	a := ahead.AgentReliability("ahead")
	b := behind.AgentReliability("behind")
	if a >= b {
		t.Fatalf("expected the near-tie to flip after one challenge; ahead %.3f vs behind %.3f", a, b)
	}
	if b-a > 0.05 {
		t.Fatalf("near-tie reversed by %.3f, more than one event should move it", b-a)
	}
}

func TestOneEventMovesBoundLittle(t *testing.T) {
	before := Reliability(Counts{Confirmed: 10})
	after := Reliability(Counts{Confirmed: 10, Challenged: 1})
	if d := before - after; d < 0 || d > 0.1 {
		t.Fatalf("one challenge moved the bound by %.3f; want a small move in (0, 0.1]", d)
	}
}

func TestBoundIsMonotone(t *testing.T) {
	prev := Reliability(Counts{})
	for n := 1; n <= 40; n++ {
		if cur := Reliability(Counts{Confirmed: n}); cur < prev {
			t.Fatalf("adding a confirmation lowered the bound at n=%d", n)
		}
		prev = Reliability(Counts{Confirmed: n})
	}
	prev = Reliability(Counts{Confirmed: 40})
	for n := 1; n <= 40; n++ {
		if cur := Reliability(Counts{Confirmed: 40, Challenged: n}); cur > prev {
			t.Fatalf("adding a challenge raised the bound at n=%d", n)
		}
		prev = Reliability(Counts{Confirmed: 40, Challenged: n})
	}
}

func TestRetentionOrderMatchesSpec(t *testing.T) {
	facts := []Fact{
		{ID: "young-untouched", AgeDays: 10},
		{ID: "old-untouched", AgeDays: 400},
		{ID: "old-recalled-once", AgeDays: 500, Recalled: 1},
		{ID: "old-used-twice", AgeDays: 600, Used: 2},
		// Protected classes: never proposed, whatever their age.
		{ID: "ancient-used-three", AgeDays: 2000, Used: 3},
		{ID: "human", AgeDays: 900, Human: true},
		{ID: "immutable", AgeDays: 900, Immutable: true},
		{ID: "open", AgeDays: 900, Open: true},
		{ID: "superseded", AgeDays: 900, Superseded: true},
		{ID: "private", AgeDays: 900, Private: true},
		{ID: "important", AgeDays: 900, Importance: 4},
	}
	got := ProposalOrder(facts)
	want := []string{"old-untouched", "young-untouched", "old-recalled-once", "old-used-twice"}
	if len(got) != len(want) {
		t.Fatalf("proposals = %v, want %v", ids(got), want)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("proposals = %v, want %v", ids(got), want)
		}
	}
}

func TestProtectedClassesNeverProposed(t *testing.T) {
	cases := []Fact{
		{ID: "immutable", Immutable: true, AgeDays: 1000},
		{ID: "human", Human: true, AgeDays: 1000},
		{ID: "open", Open: true, AgeDays: 1000},
		{ID: "superseded", Superseded: true, AgeDays: 1000},
		{ID: "private", Private: true, AgeDays: 1000},
		{ID: "important", Importance: 5, AgeDays: 1000},
		{ID: "used", Used: UseProtect, AgeDays: 1000},
	}
	for _, f := range cases {
		if p, why := Protected(f); !p {
			t.Errorf("%s: Protected = false, want true", f.ID)
		} else if why == "" {
			t.Errorf("%s: protected without a reason", f.ID)
		}
		if out := ProposalOrder([]Fact{f}); len(out) != 0 {
			t.Errorf("%s was proposed", f.ID)
		}
	}
}

func TestRetentionScoreIsZeroWithoutActivity(t *testing.T) {
	if s := RetentionScore(Fact{AgeDays: 999}); s != 0 {
		t.Fatalf("score of an untouched fact = %v, want 0 (age is a tie-break, not score)", s)
	}
}

func TestStorePersistsAndAgreesWithSnapshot(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	var evs []Event
	for i := 0; i < 7; i++ {
		evs = append(evs, Confirmed)
	}
	evs = append(evs, Challenged, Deleted, SupersededByHuman)
	var snap Snapshot
	for i, ev := range evs {
		if err := st.Bump("writer", ev); err != nil {
			t.Fatal(err)
		}
		snap.Facts = append(snap.Facts, Fact{Agent: "writer", ID: string(rune('a' + i)), Events: []Event{ev}})
	}
	if err := st.Bump("", Confirmed); err != nil {
		t.Fatal(err)
	}
	want := snap.AgentReliability("writer")
	got, err := st.Reliability("writer")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("persisted reliability %.6f != snapshot %.6f", got, want)
	}
	rows, err := st.Agents()
	if err != nil || len(rows) != 1 || rows[0].Confirmed != 7 || rows[0].Challenged != 1 {
		t.Fatalf("Agents() = %+v, %v", rows, err)
	}
	// Reopening is idempotent and sees the same counters.
	if _, err := Open(d); err != nil {
		t.Fatal(err)
	}
	if c, _ := st.Counts("nobody"); c != (Counts{}) {
		t.Fatalf("unknown agent counts = %+v", c)
	}
}

func TestStoreRecallCounter(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := st.NoteRecalled("memory/ops.md", "a1", "b2"); err != nil {
			t.Fatal(err)
		}
	}
	if n, _ := st.Recalled("memory/ops.md", "a1"); n != 3 {
		t.Fatalf("recalled = %d, want 3", n)
	}
	if n, _ := st.Recalled("memory/ops.md", "never"); n != 0 {
		t.Fatalf("unrecalled = %d, want 0", n)
	}
}

func ids(fs []Fact) []string {
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.ID
	}
	return out
}
