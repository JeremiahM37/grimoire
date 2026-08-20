package memory

import (
	"strings"
	"testing"
	"time"
)

func TestParseFormatRoundTrip(t *testing.T) {
	cases := []Entry{
		{ID: "abc123", Stamp: "2026-08-14 09:00", Agent: "claude", Text: "user prefers tabs"},
		{ID: "abc124", Stamp: "2026-08-14 09:00", Agent: "claude", Task: "refactor api",
			Text: "the deploy script lives at /usr/local/bin/deploy.sh"},
		{ID: "abc125", Stamp: "2026-08-14 09:00", Agent: "agent", Session: "run-7",
			Category: "preference", Text: "prefers dark mode"},
		{ID: "abc126", Stamp: "2026-08-14 09:00", Agent: "agent",
			Expires: "2026-09-01T00:00:00Z", Text: "on call until september"},
		{ID: "abc127", Stamp: "2026-08-14 09:00", Agent: "agent", Immutable: true,
			Text: "never delete the production database"},
		{ID: "abc128", Stamp: "2026-08-14 09:00", Agent: "agent", SupersededBy: "abc129",
			Text: "user prefers spaces"},
		{ID: "abc130", Stamp: "2026-08-14 09:00", Agent: "agent",
			Text: "the flag is **bold** — and uses · a middot"},
		{ID: "abc131", Stamp: "2026-08-14 09:00", Agent: "agent", Session: "run seven",
			Category: "work notes", Text: "spaces in trailer values"},
	}
	for _, want := range cases {
		line := want.Format()
		got, ok := ParseLine(line)
		if !ok {
			t.Fatalf("ParseLine(%q) did not parse", line)
		}
		got.Line = want.Line
		if got != want {
			t.Errorf("round trip mismatch\n line: %s\n  got: %+v\n want: %+v", line, got, want)
		}
	}
}

func TestParseSkipsNonBullets(t *testing.T) {
	body := "# Memory: deploys\n\nSome prose a person wrote.\n\n" +
		"- **2026-08-14 09:00 · claude** — first fact <!--m id=aaa-->\n" +
		"- a plain markdown bullet, not a memory\n" +
		"- **2026-08-14 09:01 · claude** — second fact <!--m id=bbb-->\n"
	got := Parse(body)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Text != "first fact" || got[1].Text != "second fact" {
		t.Errorf("wrong texts: %q, %q", got[0].Text, got[1].Text)
	}
	if got[0].Line != 4 || got[1].Line != 6 {
		t.Errorf("wrong line numbers: %d, %d", got[0].Line, got[1].Line)
	}
}

func TestParseLegacyBulletGetsStableID(t *testing.T) {
	line := "- **2026-08-14 09:00 · claude · some task** — an old memory"
	a, ok := ParseLine(line)
	if !ok {
		t.Fatal("legacy bullet did not parse")
	}
	b, _ := ParseLine(line)
	if a.ID == "" || a.ID != b.ID {
		t.Fatalf("unstable legacy id: %q vs %q", a.ID, b.ID)
	}
	if a.Text != "an old memory" || a.Agent != "claude" || a.Task != "some task" {
		t.Errorf("legacy parse wrong: %+v", a)
	}
}

func TestDeriveIDIgnoresCosmeticDifferences(t *testing.T) {
	a := DeriveID("2026-08-14 09:00", "claude", "User prefers Tabs.")
	b := DeriveID("2026-08-14 09:00", "claude", "user prefers tabs")
	if a != b {
		t.Errorf("normalization not applied to id: %q vs %q", a, b)
	}
	c := DeriveID("2026-08-14 09:00", "claude", "user prefers spaces")
	if a == c {
		t.Error("different facts collided")
	}
}

func TestSupersededRendersStruckThrough(t *testing.T) {
	e := Entry{ID: "x1", Stamp: "2026-08-14 09:00", Agent: "a",
		Text: "old belief", SupersededBy: "x2"}
	line := e.Format()
	visible := strings.TrimSpace(strings.Split(line, "<!--m")[0])
	if !strings.Contains(line, "~~**") || !strings.HasSuffix(visible, "~~") {
		t.Fatalf("superseded entry not struck through: %s", line)
	}
	back, ok := ParseLine(line)
	if !ok {
		t.Fatal("struck entry did not parse")
	}
	if back.Text != "old belief" || back.SupersededBy != "x2" {
		t.Errorf("struck entry parsed wrong: %+v", back)
	}
}

func TestLiveExpiryAndSupersession(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	if !(Entry{ID: "a", Text: "current"}).Live(now) {
		t.Error("plain entry should be live")
	}
	if (Entry{ID: "b", Text: "old", Expires: "2026-08-19T00:00:00Z"}).Live(now) {
		t.Error("expired entry should not be live")
	}
	if !(Entry{ID: "c", Text: "later", Expires: "2026-09-19T00:00:00Z"}).Live(now) {
		t.Error("unexpired entry should be live")
	}
	if !(Entry{ID: "d", Text: "keep", Expires: "not a date"}).Live(now) {
		t.Error("unparseable expiry should not expire the entry")
	}
	if (Entry{ID: "e", Text: "replaced", SupersededBy: "f"}).Live(now) {
		t.Error("superseded entry should not be live")
	}
}

func TestRewritePreservesNonEntryLines(t *testing.T) {
	body := "# Memory: prefs\n\nprose stays\n" +
		"- **2026-08-14 09:00 · a** — one <!--m id=i1-->\n" +
		"- **2026-08-14 09:01 · a** — two <!--m id=i2-->\n\nfooter prose\n"
	entries := Parse(body)
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	entries[0].SupersededBy = "i2"
	out := Rewrite(body, entries[:1])

	if !strings.Contains(out, "# Memory: prefs") || !strings.Contains(out, "prose stays") ||
		!strings.Contains(out, "footer prose") {
		t.Errorf("non-entry lines lost:\n%s", out)
	}
	got := Parse(out)
	if len(got) != 2 {
		t.Fatalf("want 2 entries after rewrite, got %d", len(got))
	}
	if got[0].SupersededBy != "i2" {
		t.Errorf("edit not applied: %+v", got[0])
	}
	if got[1].SupersededBy != "" {
		t.Errorf("untouched entry changed: %+v", got[1])
	}
}

func TestAppendGoesAfterLastBullet(t *testing.T) {
	body := "# Memory: prefs\n\n" +
		"- **2026-08-14 09:00 · a** — one <!--m id=i1-->\n\nfooter prose\n"
	out := Append(body, Entry{ID: "i9", Stamp: "2026-08-14 09:02", Agent: "a", Text: "new one"})
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	bulletIdx, footerIdx := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "new one") {
			bulletIdx = i
		}
		if strings.Contains(l, "footer prose") {
			footerIdx = i
		}
	}
	if bulletIdx < 0 || footerIdx < 0 || bulletIdx > footerIdx {
		t.Fatalf("append placed the bullet wrongly (bullet=%d footer=%d):\n%s",
			bulletIdx, footerIdx, out)
	}
	if len(Parse(out)) != 2 {
		t.Errorf("append did not produce a parseable entry:\n%s", out)
	}
}

func TestAppendToBodyWithNoBullets(t *testing.T) {
	out := Append("# Memory: empty\n", Entry{ID: "z", Stamp: "s", Agent: "a", Text: "first"})
	got := Parse(out)
	if len(got) != 1 || got[0].Text != "first" {
		t.Fatalf("append to empty body failed:\n%s", out)
	}
}

func TestSortByRecency(t *testing.T) {
	entries := []Entry{
		{ID: "b", Stamp: "2026-08-14 09:00"},
		{ID: "c", Stamp: "2026-08-16 09:00"},
		{ID: "a", Stamp: "2026-08-14 09:00"},
	}
	SortByRecency(entries)
	if entries[0].ID != "c" || entries[1].ID != "a" || entries[2].ID != "b" {
		t.Errorf("wrong order: %v", []string{entries[0].ID, entries[1].ID, entries[2].ID})
	}
}

func TestBelievedAt(t *testing.T) {
	at := func(s string) time.Time {
		tm, err := time.ParseInLocation(StampFormat, s, time.Local)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	e := Entry{ID: "a", Text: "belief", Stamp: "2026-08-14 09:00",
		SupersededBy: "b", SupersededAt: "2026-08-18 09:00"}

	if e.BelievedAt(at("2026-08-13 09:00")) {
		t.Error("believed before it was written")
	}
	if !e.BelievedAt(at("2026-08-16 09:00")) {
		t.Error("not believed while it stood")
	}
	if !e.BelievedAt(at("2026-08-14 09:00")) {
		t.Error("not believed at the instant it was written")
	}
	if e.BelievedAt(at("2026-08-19 09:00")) {
		t.Error("still believed after being replaced")
	}
	if e.BelievedAt(at("2026-08-18 09:00")) {
		t.Error("still believed at the instant it was replaced")
	}
}

func TestBelievedAtWithoutASupersessionTime(t *testing.T) {
	// A replaced fact with no recorded replacement time must not read as
	// still-believed: hiding it from a historical view is recoverable,
	// resurrecting a retracted belief is not.
	e := Entry{ID: "a", Text: "belief", Stamp: "2026-08-14 09:00", SupersededBy: "b"}
	tm, _ := time.ParseInLocation(StampFormat, "2026-08-16 09:00", time.Local)
	if e.BelievedAt(tm) {
		t.Error("a supersession with no timestamp was treated as not yet applied")
	}
}

func TestBelievedAtRespectsExpiry(t *testing.T) {
	e := Entry{ID: "a", Text: "on call", Stamp: "2026-08-14 09:00",
		Expires: "2026-08-16T00:00:00Z"}
	before, _ := time.Parse(time.RFC3339, "2026-08-15T00:00:00Z")
	after, _ := time.Parse(time.RFC3339, "2026-08-17T00:00:00Z")
	if !e.BelievedAt(before) {
		t.Error("not believed before expiry")
	}
	if e.BelievedAt(after) {
		t.Error("still believed after expiry")
	}
}

func TestSupersededAtRoundTrips(t *testing.T) {
	want := Entry{ID: "a1", Stamp: "2026-08-14 09:00", Agent: "claude",
		Text: "a belief", SupersededBy: "b2", SupersededAt: "2026-08-18 09:00"}
	got, ok := ParseLine(want.Format())
	if !ok {
		t.Fatal("did not parse")
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}
