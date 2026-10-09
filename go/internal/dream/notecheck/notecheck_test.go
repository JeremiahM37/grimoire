package notecheck

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// now is fixed so age-based checks are deterministic.
var now = time.Date(2026, 10, 8, 12, 0, 0, 0, time.Local)

// note builds a memory note body the way remember writes one.
func note(title string, entries ...memory.Entry) string {
	body := "# Memory: " + title + "\n"
	for _, e := range entries {
		body = memory.Append(body, e)
	}
	return body
}

// agentEntry is a bullet an agent wrote, with the id the write path mints.
func agentEntry(stamp, text string) memory.Entry {
	return memory.Entry{ID: memory.DeriveID(stamp, "claude", text), Stamp: stamp, Agent: "claude", Text: text}
}

// hand appends a bullet a person typed: no trailer, so no id.
func hand(body, line string) string { return body + line + "\n" }

func doc(path, body string) dream.Doc {
	return dream.Doc{Path: path, Body: body, Kind: dream.KindMemoryNote}
}

// lineOf is the 1-based line holding text, so expectations do not hard-code
// where Append happened to put a bullet.
func lineOf(body, text string) int {
	for i, l := range strings.Split(body, "\n") {
		if strings.Contains(l, text) {
			return i + 1
		}
	}
	panic("no line containing " + text)
}

// keys renders findings as "check path:line severity [<- related]" so a table
// can state the whole expected result in one line each.
func keys(fs []dream.Finding) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		k := fmt.Sprintf("%s %s:%d %s", f.Check, f.Path, f.Line, f.Severity)
		if len(f.Related) > 0 {
			k += " <- " + strings.Join(f.Related, ", ")
		}
		out = append(out, k)
	}
	return out
}

func expect(t *testing.T, got []dream.Finding, want ...string) {
	t.Helper()
	g := strings.Join(keys(got), "\n")
	w := strings.Join(want, "\n")
	if g != w {
		t.Errorf("findings mismatch\n got:\n%s\nwant:\n%s\nmessages: %+v", g, w, got)
	}
}

func TestDuplicate(t *testing.T) {
	deploys := note("deploys", agentEntry("2026-08-01 09:00", "user prefers tabs"))
	prefs := note("prefs", agentEntry("2026-08-05 10:00", "User prefers tabs."))
	idA := memory.DeriveID("2026-08-01 09:00", "claude", "user prefers tabs")

	sameNote := note("prefs",
		agentEntry("2026-08-01 09:00", "prefers dark mode"),
		agentEntry("2026-08-02 09:00", "prefers dark mode"))
	sameNoteSecond := lineOf(sameNote, "2026-08-02 09:00")
	sameNoteFirstID := memory.DeriveID("2026-08-01 09:00", "claude", "prefers dark mode")

	// The earlier copy is the hand-typed one; the later agent write is flagged.
	deployBody := note("deploys", agentEntry("2026-08-01 09:00", "the deploy key lives in the vault"))
	handLine := "- **2026-06-01 10:00 · me** — The deploy key lives in the vault."
	handBody := hand("# Memory: hand\n", handLine)
	handID := memory.DeriveID("2026-06-01 10:00", "me", "The deploy key lives in the vault.")
	agentLine := lineOf(deployBody, "2026-08-01 09:00")

	superseded := func() memory.Entry {
		e := agentEntry("2026-08-01 09:00", "uses postgres")
		e.SupersededBy = "ffffffffffff"
		e.SupersededAt = "2026-08-20 09:00"
		return e
	}

	tests := []struct {
		name string
		docs []dream.Doc
		want []string
	}{
		{
			name: "same text across two notes flags the later one",
			docs: []dream.Doc{doc("memory/deploys.md", deploys), doc("memory/prefs.md", prefs)},
			want: []string{
				fmt.Sprintf("duplicate memory/prefs.md:%d low <- memory/deploys.md#%s", lineOf(prefs, "User prefers tabs"), idA),
			},
		},
		{
			name: "same text twice in one note flags the second",
			docs: []dream.Doc{doc("memory/prefs.md", sameNote)},
			want: []string{
				fmt.Sprintf("duplicate memory/prefs.md:%d low <- memory/prefs.md#%s", sameNoteSecond, sameNoteFirstID),
			},
		},
		{
			name: "superseded copy is ignored",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-07-01 09:00", "uses postgres"))),
				doc("memory/b.md", note("b", superseded())),
			},
			want: nil,
		},
		{
			name: "hand-written bullet still counts, and orders by stamp",
			docs: []dream.Doc{doc("memory/deploys.md", deployBody), doc("memory/hand.md", handBody)},
			want: []string{
				fmt.Sprintf("duplicate memory/deploys.md:%d low <- memory/hand.md#%s", agentLine, handID),
			},
		},
		{
			name: "bank documents are not memory notes",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-08-01 09:00", "user prefers tabs"))),
				{Path: "banks/x.md", Body: "- **2026-08-05 10:00 · claude** — user prefers tabs <!--m id=zz-->\n", Kind: dream.KindBank},
			},
			want: nil,
		},
		{
			name: "punctuation and case alone are the same fact",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-08-01 09:00", "The Build Uses Make!"))),
				doc("memory/b.md", note("b", agentEntry("2026-08-02 09:00", "the build uses make"))),
			},
			want: []string{
				fmt.Sprintf("duplicate memory/b.md:%d low <- memory/a.md#%s",
					lineOf(note("b", agentEntry("2026-08-02 09:00", "the build uses make")), "2026-08-02"),
					memory.DeriveID("2026-08-01 09:00", "claude", "The Build Uses Make!")),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			expect(t, Check(tc.docs, now), tc.want...)
		})
	}
}

func TestNearDuplicate(t *testing.T) {
	a := "the deploy pipeline runs nightly on the media host"
	b := "the deploy pipeline runs nightly on the media server host"
	bodyA := note("a", agentEntry("2026-08-01 09:00", a))
	bodyB := note("b", agentEntry("2026-08-02 09:00", b))
	idA := memory.DeriveID("2026-08-01 09:00", "claude", a)

	tests := []struct {
		name string
		docs []dream.Doc
		want []string
	}{
		{
			name: "high overlap with both sides long enough",
			docs: []dream.Doc{doc("memory/a.md", bodyA), doc("memory/b.md", bodyB)},
			want: []string{
				fmt.Sprintf("near_duplicate memory/b.md:%d info <- memory/a.md#%s", lineOf(bodyB, "2026-08-02"), idA),
			},
		},
		{
			name: "short fact cannot near-duplicate, even at full overlap",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-08-01 09:00", "deploy pipeline runs nightly"))),
				doc("memory/b.md", note("b", agentEntry("2026-08-02 09:00", "deploy pipeline runs nightly now"))),
			},
			want: nil,
		},
		{
			name: "overlap below threshold",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-08-01 09:00", "the deploy pipeline runs nightly on the media host"))),
				doc("memory/b.md", note("b", agentEntry("2026-08-02 09:00", "the deploy pipeline runs weekly on the backup host"))),
			},
			want: nil,
		},
		{
			name: "exact duplicates are left to the duplicate check",
			docs: []dream.Doc{
				doc("memory/a.md", note("a", agentEntry("2026-08-01 09:00", "user prefers tabs over spaces in go code"))),
				doc("memory/b.md", note("b", agentEntry("2026-08-02 09:00", "user prefers tabs over spaces in go code"))),
			},
			want: []string{
				fmt.Sprintf("duplicate memory/b.md:%d low <- memory/a.md#%s",
					lineOf(note("b", agentEntry("2026-08-02 09:00", "x")), "2026-08-02"),
					memory.DeriveID("2026-08-01 09:00", "claude", "user prefers tabs over spaces in go code")),
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Check(tc.docs, now)
			var filtered []dream.Finding
			for _, f := range got {
				if f.Check == "near_duplicate" || f.Check == "duplicate" {
					filtered = append(filtered, f)
				}
			}
			expect(t, filtered, tc.want...)
		})
	}
}

func TestExpired(t *testing.T) {
	past := agentEntry("2026-08-01 09:00", "on call until september")
	past.Expires = "2026-09-01T00:00:00Z"
	future := agentEntry("2026-08-01 09:00", "on leave until december")
	future.Expires = "2026-12-01T00:00:00Z"
	pastSuperseded := agentEntry("2026-08-01 09:00", "staging is at the old address")
	pastSuperseded.Expires = "2026-09-01T00:00:00Z"
	pastSuperseded.SupersededBy = "ffffffffffff"
	malformed := agentEntry("2026-08-01 09:00", "trial ends soon")
	malformed.Expires = "next tuesday"

	tests := []struct {
		name string
		body string
		want []string
	}{
		{"active and past expiry", note("t", past), []string{fmt.Sprintf("expired memory/t.md:%d low", lineOf(note("t", past), "on call"))}},
		{"expiry in the future", note("t", future), nil},
		{"past expiry but superseded", note("t", pastSuperseded), nil},
		{"unparseable expiry is not expired", note("t", malformed), nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Check([]dream.Doc{doc("memory/t.md", tc.body)}, now)
			var filtered []dream.Finding
			for _, f := range got {
				if f.Check == "expired" {
					filtered = append(filtered, f)
				}
			}
			expect(t, filtered, tc.want...)
		})
	}
}

func TestOpenChallenge(t *testing.T) {
	old := agentEntry("2026-09-01 10:00", "the staging database is on the old host")
	old.Challenges = "aaaaaaaaaaaa"
	recent := agentEntry("2026-10-01 10:00", "the staging database is on the new host")
	recent.Challenges = "bbbbbbbbbbbb"

	body := note("c", old)
	got := Check([]dream.Doc{doc("memory/c.md", body)}, now)
	expect(t, got, fmt.Sprintf("open_challenge memory/c.md:%d medium <- aaaaaaaaaaaa", lineOf(body, "old host")))
	if len(got) != 1 || !strings.Contains(got[0].Message, "open 37 days") {
		t.Errorf("want open-for-37-days message, got %+v", got)
	}
	if len(got) == 1 && (len(got[0].Related) != 1 || got[0].Related[0] != "aaaaaaaaaaaa") {
		t.Errorf("Related should be the challenged id, got %v", got[0].Related)
	}

	body = note("c", recent)
	got = Check([]dream.Doc{doc("memory/c.md", body)}, now)
	expect(t, got, fmt.Sprintf("open_challenge memory/c.md:%d medium <- bbbbbbbbbbbb", lineOf(body, "new host")))
	if len(got) == 1 && strings.Contains(got[0].Message, "open ") {
		t.Errorf("a challenge under 14 days should not carry an age, got %q", got[0].Message)
	}

	superseded := old
	superseded.SupersededBy = "ffffffffffff"
	expect(t, Check([]dream.Doc{doc("memory/c.md", note("c", superseded))}, now), nil...)
}

func TestUnhelpful(t *testing.T) {
	bad := agentEntry("2026-08-01 09:00", "prefers the old cli flags")
	bad.Unhelpful = 2
	mixed := agentEntry("2026-08-01 09:00", "prefers the new cli flags")
	mixed.Helpful = 3
	mixed.Unhelpful = 5
	close := agentEntry("2026-08-01 09:00", "prefers verbose logs")
	close.Helpful = 2
	close.Unhelpful = 3

	tests := []struct {
		name  string
		entry memory.Entry
		want  []string
	}{
		{"two more unhelpful than helpful", bad, []string{"unhelpful memory/u.md:2 low"}},
		{"net of exactly two", mixed, []string{"unhelpful memory/u.md:2 low"}},
		{"net of one is not enough", close, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Check([]dream.Doc{doc("memory/u.md", note("u", tc.entry))}, now)
			expect(t, got, tc.want...)
		})
	}
}

func TestStaleVolatile(t *testing.T) {
	tests := []struct {
		name string
		text string
		when string
		want []string
	}{
		{"ip address, old", "the staging API runs at 10.0.0.12", "2026-06-01 10:00", []string{"stale_volatile memory/s.md:2 info"}},
		{"host with port, old", "the metrics endpoint is localhost:9090", "2026-06-01 10:00", []string{"stale_volatile memory/s.md:2 info"}},
		{"semver, old", "the bridge is pinned at v2.4.1", "2026-06-01 10:00", []string{"stale_volatile memory/s.md:2 info"}},
		{"snapshot word, old", "currently prefers tabs", "2026-06-01 10:00", []string{"stale_volatile memory/s.md:2 info"}},
		{"no volatile value, old", "prefers tabs over spaces in go code", "2026-06-01 10:00", nil},
		{"volatile value, recent", "the staging API runs at 10.0.0.12", "2026-09-20 10:00", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := agentEntry(tc.when, tc.text)
			got := Check([]dream.Doc{doc("memory/s.md", note("s", e))}, now)
			expect(t, got, tc.want...)
		})
	}

	got := Check([]dream.Doc{doc("memory/s.md", note("s", agentEntry("2026-06-01 10:00", "the bridge is pinned at v2.4.1")))}, now)
	if len(got) != 1 || !strings.Contains(got[0].Message, "last asserted 129 days ago") {
		t.Errorf("want age in message, got %+v", got)
	}
}

func TestOrderingAndFilters(t *testing.T) {
	stale := agentEntry("2026-06-01 10:00", "the bridge is pinned at v2.4.1")
	dup1 := agentEntry("2026-08-01 09:00", "prefers dark mode everywhere")
	dup2 := agentEntry("2026-08-03 09:00", "prefers dark mode everywhere")
	ch := agentEntry("2026-08-02 09:00", "the cache is in redis")
	ch.Challenges = "aaaaaaaaaaaa"

	docs := []dream.Doc{
		doc("memory/z.md", note("z", stale)),
		doc("memory/a.md", note("a", dup1)),
		doc("memory/m.md", note("m", dup2, ch)),
		{Path: "banks/b.md", Body: "- **2026-08-03 09:00 · claude** — prefers dark mode everywhere <!--m id=x-->\n", Kind: dream.KindBank},
	}
	got := Check(docs, now)
	// Medium before Low before Info; within a severity, by path, then line.
	wantChecks := []string{"open_challenge", "duplicate", "stale_volatile"}
	var checks []string
	for _, f := range got {
		checks = append(checks, f.Check)
		if f.Category != dream.Hygiene {
			t.Errorf("%s: category %q, want hygiene", f.Check, f.Category)
		}
	}
	if strings.Join(checks, ",") != strings.Join(wantChecks, ",") {
		t.Errorf("order: got %v want %v", checks, wantChecks)
	}

	if out := Check(nil, now); len(out) != 0 {
		t.Errorf("no docs should give no findings, got %+v", out)
	}
}
