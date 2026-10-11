package index

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Retroactive importance (capture). Every test here turns the flag on with
// exp.Set and restores it, and none runs in parallel.

// plantedStart is the first of the 300 planted writes, two hours apart. Slot 200
// is the human correction; slots before 117 are outside the seven-day window.
var plantedStart = fixedNow.Add(-(300*2*time.Hour + 48*time.Hour))

const (
	plantedSlots     = 300
	plantedEventSlot = 200
	plantedEventID   = "ev-correction"
)

func slotStamp(slot int) string {
	return plantedStart.Add(time.Duration(slot) * 2 * time.Hour).Format(memory.StampFormat)
}

// plantedKind is the planted relation of one write to the correction. The
// label is written by the test from the timeline, never read back from the
// implementation.
type plantedKind string

const (
	kindPosNote     plantedKind = "related-same-note"
	kindPosSession  plantedKind = "related-same-session"
	kindPosEntity   plantedKind = "related-shared-entity"
	kindNegUnrel    plantedKind = "unrelated-same-time"
	kindNegWindow   plantedKind = "outside-window"
	kindNegPulled   plantedKind = "pulled"
	kindNegHidden   plantedKind = "hidden"
	kindNegImportan plantedKind = "already-important"
	kindEvent       plantedKind = "event"
	kindPredecessor plantedKind = "predecessor"
	kindFiller      plantedKind = "filler"
)

// plantedWrite is one write of the timeline and the note it lands in.
type plantedWrite struct {
	note  string
	entry memory.Entry
	kind  plantedKind
}

// plantedTimeline builds the 300 writes. Roles rotate by slot, so the layout is
// fixed and needs no random seed.
func plantedTimeline() []plantedWrite {
	const media, ops, home = "memory/media.md", "memory/ops.md", "memory/home.md"
	var out []plantedWrite
	add := func(slot int, note string, kind plantedKind, imp int, session, text string, tweak func(*memory.Entry)) {
		id := fmt.Sprintf("s%03d", slot)
		e := entry(id, slotStamp(slot), text)
		e.Session = session
		e.Importance = imp
		if tweak != nil {
			tweak(&e)
		}
		out = append(out, plantedWrite{note: note, entry: e, kind: kind})
	}
	for slot := 0; slot < plantedSlots; slot++ {
		switch {
		case slot == 20:
			// The belief the correction replaces. Outside the window.
			add(slot, home, kindPredecessor, 3, "sess-old", "the Sonarr service listens on port 8989", func(e *memory.Entry) {
				e.SupersededBy = plantedEventID
				e.SupersededAt = slotStamp(plantedEventSlot)
			})
		case slot == plantedEventSlot:
			add(slot, media, kindEvent, 0, "sess-A", "the Sonarr service listens on port 8990 now", func(e *memory.Entry) {
				e.ID = plantedEventID
				e.Human = true
			})
		case slot < 117:
			// Outside the window, same note or session as the correction.
			if slot%2 == 0 {
				add(slot, media, kindNegWindow, 2, "sess-M", "media library scan finished nominally", nil)
			} else {
				add(slot, ops, kindNegWindow, 1, "sess-A", "deploy window stayed open", nil)
			}
		case slot >= 201 && slot < 210:
			// After the correction: same session, or same note.
			if slot%2 == 1 {
				add(slot, ops, kindPosSession, 1, "sess-A", "deploy window stayed open", nil)
			} else {
				add(slot, media, kindPosNote, 2, "sess-M", "media library scan finished nominally", nil)
			}
		case slot >= 117 && slot < 190:
			switch slot % 4 {
			case 0:
				add(slot, media, kindPosNote, 2, "sess-M", "media library scan finished nominally", nil)
			case 1:
				add(slot, home, kindNegUnrel, 2, "sess-D", "kettle descaled on tuesday", nil)
			case 2:
				add(slot, ops, kindNegUnrel, 1, "sess-E", "backup rotation ran at night", nil)
			default:
				add(slot, home, kindNegUnrel, 2, "sess-F", "the Plex library refreshed", nil)
			}
		case slot >= 190 && slot < 200:
			switch slot % 5 {
			case 0:
				add(slot, ops, kindPosSession, 1, "sess-A", "deploy window stayed open", nil)
			case 1:
				add(slot, ops, kindNegPulled, 1, "sess-A", "release notes say the queue moved", func(e *memory.Entry) {
					e.Origin = "https://news.example.com/feed"
				})
			case 2:
				add(slot, ops, kindNegHidden, 1, "sess-A", "private vault key rotation note", func(e *memory.Entry) {
					e.Visibility = memory.VisPrivate
				})
			case 3:
				add(slot, ops, kindNegImportan, 4, "sess-A", "the on-call rota changed", nil)
			default:
				add(slot, home, kindPosEntity, 2, "sess-C", "the Sonarr indexer sync ran", nil)
			}
		default:
			// Background: unrated, different session, different note, no shared entity.
			note := ops
			if slot%2 == 0 {
				note = home
			}
			add(slot, note, kindFiller, 0, "sess-bg", fmt.Sprintf("routine check %d finished", slot), nil)
		}
	}
	return out
}

// writePlanted writes the timeline into an index, one note at a time.
func writePlanted(t *testing.T, ix *Index, writes []plantedWrite) {
	t.Helper()
	byNote := map[string][]memory.Entry{}
	var order []string
	for _, w := range writes {
		if _, ok := byNote[w.note]; !ok {
			order = append(order, w.note)
		}
		byNote[w.note] = append(byNote[w.note], w.entry)
	}
	for _, n := range order {
		memNote(t, ix, n, byNote[n]...)
	}
}

// captureTable reads back every derived boost, keyed by "note#id".
func captureTable(t *testing.T, ix *Index) map[string]captureRow {
	t.Helper()
	caps, err := ix.captureScores()
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]captureRow{}
	for k, c := range caps {
		out[k.note+"#"+k.id] = c
	}
	return out
}

// plantedSet returns the ids of writes with the given planted kinds.
func plantedSet(writes []plantedWrite, kinds ...plantedKind) map[string]bool {
	want := map[plantedKind]bool{}
	for _, k := range kinds {
		want[k] = true
	}
	out := map[string]bool{}
	for _, w := range writes {
		if want[w.kind] {
			out[w.entry.ID] = true
		}
	}
	return out
}

func TestCaptureFlagOffWritesNoBoostAndLeavesPruneAlone(t *testing.T) {
	ix := testIndex(t)
	writes := plantedTimeline()
	writePlanted(t, ix, writes)
	// Without the flag no write creates the derived table. Checked before any
	// helper runs, because the read helper creates it on first use.
	if n, _ := ix.DB.Count("SELECT COUNT(*) FROM sqlite_master WHERE name='memory_capture'"); n != 0 {
		t.Fatalf("flag off created memory_capture")
	}
	if caps := captureTable(t, ix); len(caps) != 0 {
		t.Fatalf("flag off must derive no boosts, got %d", len(caps))
	}
}

func TestPlantedTimelinePromotesExactlyTheRelatedWrites(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	writes := plantedTimeline()
	writePlanted(t, ix, writes)

	planted := plantedSet(writes, kindPosNote, kindPosSession, kindPosEntity)
	negatives := plantedSet(writes, kindNegUnrel, kindNegWindow, kindNegPulled,
		kindNegHidden, kindNegImportan, kindFiller)

	promoted := map[string]bool{}
	for key := range captureTable(t, ix) {
		id := key[strings.Index(key, "#")+1:]
		promoted[id] = true
	}
	var tp, fp, fn []string
	for id := range promoted {
		if planted[id] {
			tp = append(tp, id)
		} else {
			fp = append(fp, id)
		}
	}
	for id := range planted {
		if !promoted[id] {
			fn = append(fn, id)
		}
	}
	sort.Strings(fp)
	sort.Strings(fn)
	precision := float64(len(tp)) / float64(len(tp)+len(fp))
	recall := float64(len(tp)) / float64(len(planted))
	t.Logf("planted related=%d promoted=%d tp=%d fp=%d fn=%d precision=%.3f recall=%.3f",
		len(planted), len(promoted), len(tp), len(fp), len(fn), precision, recall)
	if len(fp) > 0 {
		t.Errorf("unrelated or excluded writes were promoted: %v", fp)
	}
	if len(fn) > 0 {
		t.Errorf("related writes were not promoted: %v", fn)
	}
	for id := range promoted {
		if negatives[id] {
			t.Errorf("planted negative %s was promoted", id)
		}
	}
	if len(planted) == 0 {
		t.Fatal("the planted timeline has no related writes; the test would be vacuous")
	}
}

func TestCaptureDecaysWithDistanceFromTheEvent(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	writePlanted(t, ix, plantedTimeline())
	caps := captureTable(t, ix)

	// Same-session writes on both sides of the correction, ordered by distance.
	type point struct {
		dtHours float64
		score   float64
	}
	var pts []point
	for slot := 190; slot < 210; slot++ {
		if slot == plantedEventSlot {
			continue
		}
		c, ok := caps[fmt.Sprintf("memory/ops.md#s%03d", slot)]
		if !ok {
			continue
		}
		if c.reason != "session" {
			continue
		}
		dt := math.Abs(float64(slot-plantedEventSlot)) * 2
		pts = append(pts, point{dt, c.score})
		want := math.Pow(0.5, dt/captureHalfLife.Hours())
		if math.Abs(c.score-want) > 1e-9 {
			t.Errorf("slot %d: score %.12f, want 0.5^(dt/half-life) = %.12f", slot, c.score, want)
		}
	}
	if len(pts) < 4 {
		t.Fatalf("too few session boosts to test decay: %d", len(pts))
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].dtHours < pts[j].dtHours })
	for i := 1; i < len(pts); i++ {
		if pts[i].dtHours > pts[i-1].dtHours && pts[i].score >= pts[i-1].score {
			t.Errorf("boost did not decrease with distance: dt %.0fh=%.4f, dt %.0fh=%.4f",
				pts[i-1].dtHours, pts[i-1].score, pts[i].dtHours, pts[i].score)
		}
	}
}

func TestCaptureRecordsItsScopeAndSource(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	writePlanted(t, ix, plantedTimeline())
	score, reason, source, err := ix.CaptureOf("memory/home.md", "s194")
	if err != nil {
		t.Fatal(err)
	}
	if reason != "entity" || score <= 0 {
		t.Fatalf("entity-linked write: reason=%q score=%v", reason, score)
	}
	if source != "memory/media.md#"+plantedEventID {
		t.Fatalf("source = %q, want the correction", source)
	}
	if score > captureEntityW+1e-12 {
		t.Fatalf("entity-only score %.4f exceeds its weight %.2f", score, captureEntityW)
	}
}

func TestSecondEventDoesNotStack(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	// A low-importance fact g, and two corrections, each alone in range of it.
	g := entry("g", slotStamp(100), "the deploy window stayed open")
	g.Session = "sess-A"
	g.Importance = 2
	ev := func(id string, slot int) memory.Entry {
		e := entry(id, slotStamp(slot), "the Sonarr service listens on port 8990 now")
		e.Human = true
		e.Session = "sess-A"
		return e
	}
	// Each correction needs a predecessor to be an event at all.
	pred := func(id string, slot int) memory.Entry {
		p := entry("p"+id, slotStamp(slot-1), "the Sonarr service listens on port 8989")
		p.SupersededBy = id
		p.SupersededAt = slotStamp(slot)
		return p
	}
	scoreWith := func(entries ...memory.Entry) float64 {
		ix := testIndex(t)
		memNote(t, ix, "memory/ops.md", entries...)
		s, _, _, err := ix.CaptureOf("memory/ops.md", "g")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	one := scoreWith(g, pred("e1", 110), ev("e1", 110))
	two := scoreWith(g, pred("e2", 140), ev("e2", 140))
	both := scoreWith(g, pred("e1", 110), ev("e1", 110), pred("e2", 140), ev("e2", 140))
	if one <= 0 || two <= 0 {
		t.Fatalf("each correction alone must boost g: one=%v two=%v", one, two)
	}
	if math.Abs(both-math.Max(one, two)) > 1e-12 {
		t.Fatalf("two events: score %.6f, want the max of %.6f and %.6f", both, one, two)
	}
	if both > 1 {
		t.Fatalf("boost exceeds its bound: %v", both)
	}
}

func TestPulledAndHiddenFactsAreNeverBoostedOrEvents(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	ev := entry("ev", slotStamp(100), "the Sonarr service listens on port 8990 now")
	ev.Human = true
	ev.Session = "sess-A"
	ev.Origin = "https://news.example.com/feed" // a pulled "correction" is not a person's
	pred := entry("old", slotStamp(90), "the Sonarr service listens on port 8989")
	pred.SupersededBy = "ev"
	pred.SupersededAt = slotStamp(100)
	pulled := entry("pulled", slotStamp(102), "the queue moved on tuesday")
	pulled.Session = "sess-A"
	pulled.Importance = 1
	pulled.Origin = "https://news.example.com/feed"
	hidden := entry("hidden", slotStamp(103), "a private vault note")
	hidden.Session = "sess-A"
	hidden.Importance = 1
	hidden.Visibility = memory.VisPrivate
	memNote(t, ix, "memory/ops.md", ev, pred, pulled, hidden)
	if caps := captureTable(t, ix); len(caps) != 0 {
		t.Fatalf("a pulled event or a pulled/hidden target was boosted: %v", caps)
	}
}

func TestUndoingTheCorrectionRemovesTheBoost(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	writes := plantedTimeline()
	writePlanted(t, ix, writes)
	if len(captureTable(t, ix)) == 0 {
		t.Fatal("nothing was promoted before the undo")
	}
	// Rewrite the note without the correction, as a person deleting the line would.
	var kept []memory.Entry
	for _, w := range writes {
		if w.note == "memory/media.md" && w.entry.ID != plantedEventID {
			kept = append(kept, w.entry)
		}
	}
	memNote(t, ix, "memory/media.md", kept...)
	memNote(t, ix, "memory/ops.md", filterNote(writes, "memory/ops.md")...)
	memNote(t, ix, "memory/home.md", filterNote(writes, "memory/home.md")...)
	if caps := captureTable(t, ix); len(caps) != 0 {
		t.Fatalf("%d boosts survived removing the correction", len(caps))
	}
}

func filterNote(writes []plantedWrite, note string) []memory.Entry {
	var out []memory.Entry
	for _, w := range writes {
		if w.note == note {
			out = append(out, w.entry)
		}
	}
	return out
}

func TestReindexReproducesTheSameBoosts(t *testing.T) {
	restore := exp.Set(captureFlag)
	defer restore()
	ix := testIndex(t)
	writePlanted(t, ix, plantedTimeline())
	before := captureTable(t, ix)
	if len(before) == 0 {
		t.Fatal("no boosts to compare")
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	after := captureTable(t, ix)
	if len(after) != len(before) {
		t.Fatalf("reindex changed the boost count: %d -> %d", len(before), len(after))
	}
	for k, c := range before {
		if after[k] != c {
			t.Fatalf("%s: %+v before reindex, %+v after", k, c, after[k])
		}
	}
}

func TestPruneProposalsWithAndWithoutCapture(t *testing.T) {
	ix := testIndex(t)
	writes := plantedTimeline()
	writePlanted(t, ix, writes)
	needed := plantedSet(writes, kindPosNote, kindPosSession, kindPosEntity)
	cutoff := fixedNow

	propose := func(on bool) map[string]bool {
		t.Helper()
		restore := exp.Set()
		if on {
			restore()
			restore = exp.Set(captureFlag)
		}
		defer restore()
		if on {
			// Boosts are derived at index time, so the flag needs a rebuild.
			if _, err := ix.Reindex(); err != nil {
				t.Fatal(err)
			}
		}
		hits, err := ix.PruneCandidates(2, cutoff, 5000)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, h := range hits {
			out[h.ID] = true
		}
		return out
	}
	off := propose(false)
	on := propose(true)

	neededOff, neededOn, protectedNeg := 0, 0, 0
	for id := range needed {
		if off[id] {
			neededOff++
		}
		if on[id] {
			neededOn++
		}
	}
	for id := range on {
		if !off[id] {
			t.Errorf("capture added a prune proposal %s", id)
		}
	}
	for id := range off {
		if !on[id] && !needed[id] {
			protectedNeg++
		}
	}
	t.Logf("prune proposals: off=%d on=%d; planted-needed proposed off=%d on=%d; unrelated facts protected=%d",
		len(off), len(on), neededOff, neededOn, protectedNeg)
	if neededOff == 0 {
		t.Fatal("prune never proposed a needed fact without capture; the simulation is vacuous")
	}
	if neededOn != 0 {
		t.Errorf("%d planted-needed facts were still proposed for pruning", neededOn)
	}
	if protectedNeg != 0 {
		t.Errorf("capture protected %d facts that were not planted as needed", protectedNeg)
	}
}

func TestCaptureRankingTermAddsTheBoostAndNothingElse(t *testing.T) {
	ix := testIndex(t)
	writePlanted(t, ix, plantedTimeline())
	q := MemoryQuery{Query: "the Sonarr indexer sync", Now: fixedNow, Limit: 500, LexicalOnly: true}

	restoreOff := exp.Set()
	off, err := ix.MemoryEntries(q)
	restoreOff()
	if err != nil {
		t.Fatal(err)
	}
	restoreOn := exp.Set(captureFlag)
	defer restoreOn()
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	on, err := ix.MemoryEntries(q)
	if err != nil {
		t.Fatal(err)
	}
	offScore := map[string]float64{}
	for _, h := range off {
		offScore[h.ID] = h.Score
	}
	checked := 0
	for _, h := range on {
		c, _, _, err := ix.CaptureOf(h.Note, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := offScore[h.ID] + wCapture*c
		if math.Abs(h.Score-want) > 1e-12 {
			t.Errorf("%s: score %.12f, want base %.12f + %.2f*%.4f", h.ID, h.Score, offScore[h.ID], wCapture, c)
		}
		if c > 0 {
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no boosted hit appeared in the ranking; the check is vacuous")
	}
}
