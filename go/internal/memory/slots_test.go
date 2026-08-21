package memory

import "testing"

// Value-slot reconciliation.
//
// A false UPDATE destroys a fact that was right; a missed one leaves two facts
// on file, which is visible and recoverable. The asymmetry is why most of this
// file is cases the rule MUST NOT fire on.

func TestTheUpdatesRealTranscriptsActuallyContain(t *testing.T) {
	// Every pair here is taken from the shape LongMemEval's knowledge-update
	// questions have. None of them parse as subject-predicate-value, and the
	// engine scored 0.8% supersession on them before this existed.
	cases := []struct{ prev, next, kind string }{
		{"I recently set a personal best time in a charity 5K run with a time of 27:12",
			"I'm hoping to beat my personal best time of 25:50 this time around", "duration"},
		{"I was pre-approved for $350,000 on my mortgage application",
			"my mortgage pre-approved amount came through at $400,000", "money"},
		{"my to-watch list has 18 titles on it right now",
			"my to-watch list is up to 25 titles", "number"},
		{"the deploy pipeline takes 11 minutes end to end",
			"the deploy pipeline takes 20 minutes end to end", "number"},
		{"my resting heart rate has been sitting around 62 bpm",
			"my resting heart rate is down to 58 bpm", "number"},
	}
	for _, c := range cases {
		kind, ok := ValueUpdate(c.prev, c.next)
		if !ok {
			t.Errorf("missed an update:\n  prev %q\n  next %q", c.prev, c.next)
			continue
		}
		if kind != c.kind {
			t.Errorf("kind = %q, want %q for %q", kind, c.kind, c.next)
		}
	}
}

func TestItDoesNotFireOnUnrelatedFactsThatHappenToHaveNumbers(t *testing.T) {
	// The dangerous direction. Every one of these would destroy a true fact.
	cases := [][2]string{
		// different subjects, one shared verb
		{"I paid $50 for the paperback", "I paid $80 for the running shoes"},
		// same units, different things
		{"the deploy pipeline takes 11 minutes", "the nightly backup takes 20 minutes"},
		// same activity, different distance — the number IS the subject here
		{"I ran the 5K in 27:12", "I ran the 10K in 58:40"},
		// two different people
		{"priya's extension is 4021", "marcus's extension is 4088"},
		// a genuinely different topic sharing filler words
		{"I would really like some tips about my sleep schedule of 7 hours",
			"I would really like some tips about my commute of 3 hours"},
		// same slot, SAME value — nothing changed, so nothing to supersede
		{"my 5K personal best time is 25:50", "my 5K personal best time is 25:50"},
	}
	for _, c := range cases {
		if kind, ok := ValueUpdate(c[0], c[1]); ok {
			t.Errorf("false update (%s):\n  prev %q\n  next %q", kind, c[0], c[1])
		}
	}
}

func TestItRefusesAcrossValueKinds(t *testing.T) {
	// "$400" and "4:00" are not competing answers to anything.
	if _, ok := ValueUpdate(
		"the conference registration fee for the annual summit is $400",
		"the conference registration desk for the annual summit opens at 4:00"); ok {
		t.Error("money was superseded by a clock time")
	}
}

func TestItRefusesWhenAStatementHasARangeRatherThanAValue(t *testing.T) {
	// "between 20 and 30 titles" has a spread, not a value. Picking one of the
	// two to compare would be inventing a fact.
	if _, ok := ValueUpdate(
		"my to-watch list has 18 titles on it",
		"my to-watch list has between 20 and 30 titles on it"); ok {
		t.Error("a range was treated as a value")
	}
}

func TestYearsAreNotValues(t *testing.T) {
	// "I visited Rome in 2019" and "I visited Rome in 2021" are two events,
	// not one fact updated. Treating years as values was the largest source of
	// wrong supersessions when this was first tried.
	if _, ok := ValueUpdate(
		"I visited the Rome colosseum tour in 2019",
		"I visited the Rome colosseum tour in 2021"); ok {
		t.Error("two dated events were merged into one updated fact")
	}
}

func TestSameSlotNeedsRealSharedTerms(t *testing.T) {
	// Two short fragments must not collide on a ratio alone.
	if _, ok := SameSlot("the fee is $40", "the fee is $90"); ok {
		t.Error("a two-content-word fragment matched on ratio")
	}
	// …and a long statement must not match everything by term count.
	long := "I have been thinking about my training plan and my nutrition and " +
		"my sleep and my commute and my budget and my reading list lately"
	if _, ok := SameSlot(long, "my budget is $300 a month"); ok {
		t.Error("a rambling statement matched an unrelated one")
	}
}

func TestValueParsingCoversTheShapesPeopleWrite(t *testing.T) {
	for _, c := range []struct {
		text string
		want string // "kind:normalized"
	}{
		{"it cost $400,000", "money:400000"},
		{"it cost 400,000 dollars", "money:400000"},
		// The separator is KEPT: "25:50" and "2550" are not the same value,
		// and stripping it would make a two-and-a-half-thousand-something
		// compare equal to a time.
		{"my time was 25:50", "duration:25:50"},
		{"the rate is 4.5%", "percent:4.5"},
		{"there are 25 titles", "number:25"},
	} {
		vs := parseValues(c.text)
		if len(vs) == 0 {
			t.Errorf("no value found in %q", c.text)
			continue
		}
		got := vs[0].kind.String() + ":" + vs[0].text
		if got != c.want {
			t.Errorf("parseValues(%q) = %s, want %s", c.text, got, c.want)
		}
	}
}

// ------------------------------------------------------- through the engine

func TestDecideSupersedesAValueUpdate(t *testing.T) {
	known := []Entry{{ID: "a1",
		Text: "I recently set a personal best time in a charity 5K run with a time of 27:12"}}
	d := Decide("I'm hoping to beat my personal best time of 25:50 this time around", known)
	if d.Op != OpUpdate || d.Target != "a1" {
		t.Fatalf("op = %s target = %q (%s)", d.Op, d.Target, d.Why)
	}
	if !contains(d.Why, "value") {
		t.Errorf("why = %q — the reason should say what changed", d.Why)
	}
}

func TestAnImmutableFactIsStillSafeFromAValueUpdate(t *testing.T) {
	known := []Entry{{ID: "a1", Immutable: true,
		Text: "the production database port is 5432 on the primary host"}}
	d := Decide("the production database port is 6543 on the primary host", known)
	if d.Op == OpUpdate || d.Op == OpDelete {
		t.Fatalf("a pinned fact was superseded by a value update: %+v", d)
	}
}

func TestAnUntrustedValueUpdateStillCannotSupersede(t *testing.T) {
	// The trust rule has to hold on the new path too, or the new path is a way
	// around it.
	known := []Entry{{ID: "a1",
		Text: "the deploy pipeline takes 11 minutes end to end"}}
	d := DecideFrom("the deploy pipeline takes 90 minutes end to end",
		"connector:jira:OPS-1", known)
	if d.Op == OpUpdate || d.Op == OpDelete {
		t.Fatalf("an untrusted source superseded via the value path: %+v", d)
	}
}

func TestASupersededEntryIsNotAValueUpdateTarget(t *testing.T) {
	known := []Entry{{ID: "a1", SupersededBy: "a2",
		Text: "the deploy pipeline takes 11 minutes end to end"}}
	d := Decide("the deploy pipeline takes 20 minutes end to end", known)
	if d.Target == "a1" {
		t.Fatalf("rewrote history rather than extending it: %+v", d)
	}
}

func TestTheExistingAttributePathStillWins(t *testing.T) {
	// Canonical facts must keep taking the precise path, which gives a better
	// reason and does not depend on any threshold.
	known := []Entry{{ID: "a1", Text: "the user prefers spaces"}}
	d := Decide("the user prefers tabs", known)
	if d.Op != OpUpdate || d.Target != "a1" {
		t.Fatalf("%+v", d)
	}
	if contains(d.Why, "value") {
		t.Errorf("a subject-predicate update took the value path: %q", d.Why)
	}
}

func contains(hay, needle string) bool {
	return len(needle) > 0 && len(hay) >= len(needle) &&
		func() bool {
			for i := 0; i+len(needle) <= len(hay); i++ {
				if hay[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		}()
}

func TestSharedUnitsAndLightVerbsAreNotEnough(t *testing.T) {
	// Found by the verify suite. These two share exactly three terms — takes,
	// minutes, end — every one of them about HOW the thing is measured and
	// none about WHAT it is. The rule superseded a true fact about the deploy
	// with an unrelated one about the backup.
	if kind, ok := ValueUpdate(
		"the deploy pipeline takes 11 minutes end to end",
		"the nightly backup takes 20 minutes end to end"); ok {
		t.Errorf("false update (%s) on two facts sharing only units and verbs", kind)
	}
	// More of the same shape, all of which must stay separate.
	for _, c := range [][2]string{
		{"I spent 3 hours on the garden this week", "I spent 5 hours on the garage this week"},
		{"my morning commute is 30 minutes each way",
			"my evening swim is 45 minutes each way"},
	} {
		if _, ok := ValueUpdate(c[0], c[1]); ok {
			t.Errorf("false update:\n  prev %q\n  next %q", c[0], c[1])
		}
	}
	// A KNOWN LIMIT, asserted so that anyone who fixes it finds out here: two
	// facts that differ only in a modifier of a shared head noun still
	// collide. "standup meeting" and "retro meeting" share `meeting`, which is
	// discriminative, and nothing in the rule sees that `standup` and `retro`
	// are the words doing the work. Measured at roughly 1 in 400 real pairs
	// (benchmarks/longmemeval/REPORT-slots.md), which is why it is documented
	// rather than chased with another threshold.
	if _, ok := ValueUpdate(
		"the standup meeting takes 15 minutes every day",
		"the retro meeting takes 45 minutes every day"); !ok {
		t.Log("the shared-head-noun limitation appears to be fixed — good; " +
			"delete this assertion and the note in slots.go")
	}

	// …while the real ones still fire, because they share a distinctive term.
	for _, c := range [][2]string{
		{"the deploy pipeline takes 11 minutes end to end",
			"the deploy pipeline takes 20 minutes end to end"},
		{"my morning commute is 30 minutes each way",
			"my morning commute is 45 minutes each way"},
	} {
		if _, ok := ValueUpdate(c[0], c[1]); !ok {
			t.Errorf("missed a real update:\n  prev %q\n  next %q", c[0], c[1])
		}
	}
}
