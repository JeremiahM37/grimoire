package adherence

import (
	"testing"
	"time"
)

func openT(t *testing.T) *Store {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestTagsAreUniquePrefixes(t *testing.T) {
	keys := []string{"3e99aaaa", "3e99bbbb", "ab12cccc"}
	tags := Tags(keys, 4)
	if tags[0] != "3e99a" || tags[1] != "3e99b" || tags[2] != "ab12" {
		t.Fatalf("tags %v", tags)
	}
}

func TestExtractTags(t *testing.T) {
	got := ExtractTags("done (m:3e99), also m:ab12cd and not xm:1234 or m:zz")
	if len(got) != 2 || got[0] != "3e99" || got[1] != "ab12cd" {
		t.Fatalf("got %v", got)
	}
}

func TestOutcomePriorityAndFinalize(t *testing.T) {
	s := openT(t)
	now := time.Now()
	s.Log([]Injection{{Session: "s", Tag: "aaaa", Key: "aaaa00", Target: "fact:1", FactID: "1"},
		{Session: "s", Tag: "bbbb", Key: "bbbb00", Target: "fact:2", FactID: "2"},
		{Session: "s", Tag: "cccc", Key: "cccc00", Target: "fact:3", FactID: "3"}}, now)
	s.MarkCited("s", []string{"aaaa", "zzzz"}, now.Add(-time.Hour))
	rows, _ := s.OpenRows("s", now.Add(-time.Hour))
	s.SetCheck(rows[1].ID, Followed)
	s.SetCheck(rows[1].ID, Violated)
	s.SetCheck(rows[1].ID, Followed) // must not undo a violation
	fin, _ := s.Finalize("s", now.Add(-time.Hour))
	want := []string{Cited, Violated, Ignored}
	for i, r := range fin {
		if r.Outcome() != want[i] {
			t.Errorf("row %d: %s want %s", i, r.Outcome(), want[i])
		}
	}
	if again, _ := s.Finalize("s", now.Add(-time.Hour)); len(again) != 0 {
		t.Fatal("finalised rows must not come back")
	}
	if r, _ := s.Contradict("fact:3", time.Hour, now); r == nil || r.Outcome() != Contradicted {
		t.Fatal("contradict")
	}
	if r, _ := s.Contradict("fact:3", time.Hour, now); r != nil {
		t.Fatal("a row is contradicted once")
	}
}

func TestRetentionPrunes(t *testing.T) {
	s := openT(t)
	old := time.Now().Add(-Retention - time.Hour)
	s.Log([]Injection{{Session: "s", Tag: "aaaa", Key: "k", Target: "fact:1"}}, old)
	s.Log([]Injection{{Session: "s", Tag: "bbbb", Key: "k2", Target: "fact:2"}}, time.Now())
	by, all, _ := s.Report(time.Unix(0, 0))
	if all.Injected != 1 || by["fact:2"] == nil {
		t.Fatalf("old row survived: %+v", all)
	}
}

func TestPenalty(t *testing.T) {
	if Penalty(Stats{Injected: 4, Ignored: 4}) != 1 {
		t.Fatal("too early")
	}
	if p := Penalty(Stats{Injected: 10, Ignored: 10}); p >= 1 || p < 0.6 {
		t.Fatalf("penalty %v", p)
	}
	if Penalty(Stats{Injected: 10, Ignored: 6, Cited: 4}) != 1 {
		t.Fatal("acted-on memories are not punished")
	}
}

func TestCheckSemantics(t *testing.T) {
	c := Check{Forbid: `\bgit\s+push\b`, Enforce: "ask", Tools: []string{"Bash"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !c.Violates("git push origin main") || c.Violates("git status") || !c.AppliesTo("bash") || c.AppliesTo("Edit") {
		t.Fatal("forbid")
	}
	if (Check{Enforce: "ask", Require: "x"}).Validate() == nil {
		t.Fatal("enforce needs forbid")
	}
	if (Check{Forbid: "("}).Validate() == nil {
		t.Fatal("bad regex")
	}
}

func TestCandidates(t *testing.T) {
	got := Candidates("Never push to main; do not run `git push --force` and never rm -rf the vault. Never use mocks.")
	if len(got) < 2 {
		t.Fatalf("got %v", got)
	}
	for _, c := range Candidates("Never use mocks in tests.") {
		t.Errorf("no pattern expected, got %s", c)
	}
}

func TestGateSummary(t *testing.T) {
	s := openT(t)
	for i := 1; i <= 100; i++ {
		s.LogGate(GateCall{MS: int64(i), Candidates: 2, Kept: 1, Dropped: 1, TimedOut: i == 100}, time.Now())
	}
	g, _ := s.Gate(time.Unix(0, 0))
	if g.Calls != 100 || g.P50MS != 51 || g.P95MS != 95 || g.TimedOut != 1 {
		t.Fatalf("%+v", g)
	}
}
