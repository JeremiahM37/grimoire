package memory

import (
	"math"
	"testing"
	"time"
)

func at(s string) time.Time {
	t, err := time.ParseInLocation(StampFormat, s, time.Local)
	if err != nil {
		panic(err)
	}
	return t
}

func TestParseFresh(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour,
		"2w": 14 * 24 * time.Hour, "90m": 90 * time.Minute, "1.5d": 36 * time.Hour} {
		if _, d, ok := ParseFresh(in); !ok || d != want {
			t.Errorf("ParseFresh(%q) = %v %v, want %v", in, d, ok, want)
		}
	}
	for _, in := range []string{"", "auto", "stable", "VOLATILE"} {
		if _, _, ok := ParseFresh(in); !ok {
			t.Errorf("ParseFresh(%q) refused", in)
		}
	}
	for _, in := range []string{"sometimes", "-3d", "0d", "30s", "7 days"} {
		if _, _, ok := ParseFresh(in); ok {
			t.Errorf("ParseFresh(%q) accepted", in)
		}
	}
}

func TestFreshnessFieldsRoundTrip(t *testing.T) {
	e := Entry{Text: "grimoire is build 1.4.0", Agent: "claude-code",
		Stamp: "2026-08-14 09:00", Fresh: "7d", Check: "grimoire version --json > /dev/null",
		Verified: "2026-08-20 10:00", Changes: 2, Verifies: 3, Since: "2026-06-01 08:00"}
	e.ID = DeriveID(e.Stamp, e.Agent, e.Text)
	line := e.Format()
	got, ok := ParseLine(line)
	if !ok {
		t.Fatalf("did not parse: %s", line)
	}
	if got.Fresh != e.Fresh || got.Check != e.Check || got.Verified != e.Verified ||
		got.Changes != 2 || got.Verifies != 3 || got.Since != e.Since || got.Text != e.Text {
		t.Errorf("round trip lost fields:\n%s\n%+v", line, got)
	}
	if got.Format() != line {
		t.Errorf("Format(Parse(x)) != x:\n%s\n%s", got.Format(), line)
	}
}

func TestShapeSetsThePrior(t *testing.T) {
	for _, s := range []string{"the router is 192.168.0.1", "lectern listens on :9110",
		"ollama is pinned at 0.20.5", "currently on the main branch", "root is 43% full"} {
		if ShapeOf(s) != ShapeVolatil {
			t.Errorf("%q not volatile-shaped", s)
		}
	}
	for _, s := range []string{"the user prefers tabs", "we chose SQLite because it runs offline"} {
		if ShapeOf(s) != ShapePlain {
			t.Errorf("%q volatile-shaped", s)
		}
	}
}

func TestHistoryOverridesThePrior(t *testing.T) {
	now := at("2026-08-31 09:00")
	p := DefaultPriors()
	quiet := Entry{Text: "the user prefers tabs", Stamp: "2026-08-01 09:00"}
	busy := Entry{Text: "the user prefers tabs", Stamp: "2026-08-30 09:00",
		Since: "2026-08-01 09:00", Changes: 4}
	if quiet.Rate(p, now) >= busy.Rate(p, now) {
		t.Errorf("a fact changed four times this month is not believed to change faster")
	}
	// Four changes in thirty days, against a two-year prior: the rate should be
	// near the observed one, not the prior.
	if r := busy.Rate(p, now); r < 1.0/20 {
		// observed span is Aug 1 to Aug 30
		t.Errorf("rate %.4f/day ignores four changes in 30 days", r)
	}
}

func TestAssessActions(t *testing.T) {
	now := at("2026-08-14 09:00")
	p := DefaultPriors()
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Text: "x", Stamp: "2026-08-14 09:00", Fresh: "volatile"}, ActionVerify},
		{Entry{Text: "port :80", Stamp: "2020-01-01 09:00", Fresh: "stable"}, ActionUse},
		{Entry{Text: "x", Stamp: "2026-08-10 09:00", Fresh: "7d"}, ActionUse},
		{Entry{Text: "x", Stamp: "2026-08-01 09:00", Fresh: "7d"}, ActionVerify},
		{Entry{Text: "x", Stamp: "2026-08-01 09:00", Fresh: "7d", Verified: "2026-08-13 09:00"}, ActionUse},
		{Entry{Text: "the router is 192.168.0.1", Stamp: "2026-04-01 09:00"}, ActionVerify},
		{Entry{Text: "the router is 192.168.0.1", Stamp: "2026-08-10 09:00"}, ActionUse},
	}
	for _, c := range cases {
		if a := c.e.Assess(now, p, 0); a.Action != c.want {
			t.Errorf("%+v → %s (%s, p=%.2f), want %s", c.e, a.Action, a.Reason, a.PStale, c.want)
		}
	}
}

func TestUntrustedCheckIsNotOffered(t *testing.T) {
	e := Entry{Text: "x", Stamp: "2026-08-14 09:00", Fresh: "volatile", Check: "cat x", Origin: "web:example.com"}
	if a := e.Assess(at("2026-08-14 09:00"), DefaultPriors(), 0); a.Check != "" {
		t.Errorf("an untrusted fact's check was offered: %q", a.Check)
	}
}

func TestLearnPriorsMovesTowardWhatTheStoreSaw(t *testing.T) {
	none := LearnPriors([2]float64{}, [2]float64{})
	if math.Abs(none[ShapePlain]-defaultRates[ShapePlain]) > 1e-12 {
		t.Errorf("no evidence changed the prior: %v", none)
	}
	// Plain facts in this store changed 50 times over 1000 fact-days.
	seen := LearnPriors([2]float64{ShapePlain: 50}, [2]float64{ShapePlain: 1000})
	if seen[ShapePlain] < 0.03 {
		t.Errorf("learned plain rate %.4f ignores 50 changes in 1000 days", seen[ShapePlain])
	}
}

func TestSuggestTier(t *testing.T) {
	now := at("2026-08-31 09:00")
	p := DefaultPriors()
	if tier, _ := (Entry{Text: "x", Stamp: "2026-08-01 09:00", Fresh: "volatile", Verifies: 6}).SuggestTier(now, p); tier != "30d" {
		t.Errorf("volatile confirmed 6x, never changed → %q, want 30d", tier)
	}
	if tier, _ := (Entry{Text: "x", Stamp: "2026-08-30 09:00", Since: "2026-08-20 09:00", Changes: 4}).SuggestTier(now, p); tier != TierVolatile {
		t.Errorf("changed 4x in 11 days → %q, want volatile", tier)
	}
	if tier, _ := (Entry{Text: "x", Stamp: "2026-08-30 09:00", Fresh: "stable", Changes: 2}).SuggestTier(now, p); tier != "auto" {
		t.Errorf("stable but changed twice → %q, want auto", tier)
	}
	if tier, _ := (Entry{Text: "x", Stamp: "2026-08-30 09:00"}).SuggestTier(now, p); tier != "" {
		t.Errorf("a fact with no history → %q, want no suggestion", tier)
	}
}

func TestPriorFromDecision(t *testing.T) {
	daily := map[string]float64{"hours": 0.05, "days": 0.8, "weeks": 0.1, "months": 0.03, "years": 0.02}
	fast := PriorFromDecision(0.9, daily)
	if fast < 1.0/10 || fast > 1.0/2 {
		t.Errorf("a confidently day-scale fact got rate %.4f/day, want about every 3 days", fast)
	}
	// The same speed answer on a fact the model doubts changes at all is
	// noise and must not set a fast rate.
	if doubted := PriorFromDecision(0.3, daily); doubted > 1.0/365 {
		t.Errorf("a doubted fact got rate %.4f/day from its speed answer", doubted)
	}
	// A little "hours" probability cannot dominate: geometric, not arithmetic.
	monthly := map[string]float64{"hours": 0.1, "days": 0.05, "weeks": 0.05, "months": 0.7, "years": 0.1}
	if r := PriorFromDecision(0.9, monthly); r > 1.0/14 {
		t.Errorf("a mostly-monthly fact got rate %.4f/day", r)
	}
	if PriorFromDecision(0, daily) != 0 {
		t.Error("no verdict should mean no prior")
	}
	e := Entry{Text: "x", Stamp: "2026-08-14 09:00", PriorRate: fast}
	if a := e.Assess(at("2026-08-18 09:00"), DefaultPriors(), 0); a.Action != ActionVerify {
		t.Errorf("a day-scale fact four days on = %+v", a)
	}
}
