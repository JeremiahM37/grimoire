package adherence

import (
	"path/filepath"
	"testing"
	"time"
)

// TestExposureRoundTrip writes shown and withheld exposures and reads them back
// through the read-only path, which is what the learner uses.
func TestExposureRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	feats := "[0.1,0.2,0.3,0.4,0.5,0,1]"
	if err := st.Log([]Injection{
		{Session: "s", Tag: "aaaa", Key: "k1", Target: "fact:1", Stage: "prompt", PWithhold: 0.2,
			Pos: 2, Slate: "slate1", Explored: true, Feats: feats},
		{Session: "s", Tag: "bbbb", Key: "k2", Target: "fact:2", Stage: "prompt"},
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.LogWithheld([]Withheld{
		{Session: "s", Tag: "cccc", Key: "k3", Target: "fact:3", Stage: "prompt", PWithhold: 0.2,
			Pos: 3, Slate: "slate1", Explored: true, Feats: feats},
	}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE injections SET cited=1 WHERE key='k1'`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, c, err := ReadExposures(ro, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if c.Injections != 2 || c.Withheld != 1 || c.Positioned != 2 || c.Slates != 1 || c.WithFeatures != 2 || c.Explored != 2 || c.Cited != 1 {
		t.Fatalf("counts %+v", c)
	}
	var shownPos, heldPos int
	for _, r := range rows {
		if r.Table == "injections" && r.Used && r.Pos == 2 && r.Feats == feats && r.Slate == "slate1" && r.PWithhold == 0.2 {
			shownPos++
		}
		if r.Table == "trace_withheld" && !r.Shown && r.Pos == 3 && r.Explored {
			heldPos++
		}
	}
	if shownPos != 1 || heldPos != 1 {
		t.Fatalf("round trip lost fields: shown %d held %d", shownPos, heldPos)
	}
}

// TestUnpositionedRowsKeepDefaults checks that a row logged without exposure
// fields reads back as not positioned, so flag-off logs do not feed the learner.
func TestUnpositionedRowsKeepDefaults(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Log([]Injection{{Session: "s", Tag: "dddd", Key: "k9", Target: "fact:9", Stage: "prompt"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	st.Close()
	ro, err := OpenReadOnly(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	rows, c, err := ReadExposures(ro, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Pos != 0 || rows[0].Slate != "" || rows[0].Feats != "" || c.Positioned != 0 {
		t.Fatalf("unpositioned row read as %+v counts %+v", rows, c)
	}
}

// TestReadOnlyNeverCreates checks that the read-only open does not create a log
// where none exists.
func TestReadOnlyNeverCreates(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "absent")
	if db, err := OpenReadOnly(dir); err == nil {
		db.Close()
		t.Fatal("read-only open of a missing log succeeded")
	}
}
