package replay

import (
	"math"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cues"
)

// vec builds a unit-ish vector in a small space from named axes.
func vec(dim int, axes ...int) []float32 {
	v := make([]float32, dim)
	for _, a := range axes {
		v[a] = 1
	}
	return Unit(v)
}

func fact(id, text string, v []float32) *Item {
	return NewItem("fact:"+id, text, []string{text}, [][]float32{v})
}

func sit(id, text string, v []float32, expect map[string]float64) Situation {
	return Situation{ID: id, Text: text, Stage: "prompt", Vec: v, Expect: expect, Source: "live"}
}

func TestRelevanceMatchesTheContextFormula(t *testing.T) {
	// cosine 0.45 is the floor, 0.80 the ceiling; overlap can carry a match.
	if Relevance(0.45, 0) != 0 || Relevance(0.80, 0) != 1 {
		t.Fatal("stretch endpoints")
	}
	if got := Relevance(0.45, 1); math.Abs(got-0.4) > 1e-9 {
		t.Fatalf("overlap blend: %v", got)
	}
	if got := CueRelevance(0.55, 0, CueLow); got != 0 {
		t.Fatalf("cue floor: %v", got)
	}
}

func TestOverlapNeedsTwoTermsForALongQuery(t *testing.T) {
	terms := Terms("deploy kestrel certificates")
	if overlap(terms, termSet("kestrel only")) != 0 {
		t.Fatal("one shared term of three must not count")
	}
	if overlap(terms, termSet("kestrel deploy notes")) == 0 {
		t.Fatal("two shared terms must count")
	}
	if overlap(nil, nil) != 1 {
		t.Fatal("no terms: full overlap, as in the endpoint")
	}
}

func TestFireRespectsMinRelLimitAndDuplicates(t *testing.T) {
	d := 8
	items := []*Item{
		fact("a", "alpha rule", vec(d, 0)),
		fact("b", "alpha rule", vec(d, 0)), // same text: dropped
		fact("c", "gamma rule", vec(d, 0, 1)),
		fact("far", "unrelated", vec(d, 5)),
	}
	sn := NewSnapshot(items, nil)
	p := Prepare(Situation{Text: "q", Vec: vec(d, 0), Limit: 5})
	fired := sn.Fire(p, sn.Score(p, nil))
	if len(fired) != 2 || fired[0].Target != "fact:a" {
		t.Fatalf("fired %v", fired)
	}
	p2 := Prepare(Situation{Text: "q", Vec: vec(d, 0), Limit: 1})
	if got := sn.Fire(p2, sn.Score(p2, nil)); len(got) != 1 {
		t.Fatalf("limit: %v", got)
	}
}

func TestLostWhenAUsefulMemoryIsEditedAway(t *testing.T) {
	d := 8
	a := fact("a", "never push to main", vec(d, 0))
	other := fact("o", "something else entirely", vec(d, 3))
	base := NewSnapshot([]*Item{a, other}, nil)
	s := sit("s1", "can you push this", vec(d, 0), map[string]float64{"fact:a": 1})
	rp := NewReplayer(base, []Situation{s, sit("s2", "unrelated", vec(d, 3), map[string]float64{"fact:o": 1})})

	// A wording edit that moves the memory to a different region.
	edited := fact("a", "branch hygiene notes", vec(d, 6))
	rep, _ := rp.Diff(Change{Upsert: []*Item{edited}})
	if rep.LostMemories != 1 || rep.LostSituations != 1 || rep.Score >= 1 {
		t.Fatalf("expected one loss: %+v", rep)
	}
	if len(rep.Diffs) != 1 || rep.Diffs[0].Lost[0].Why != "below_min_rel" {
		t.Fatalf("diffs: %+v", rep.Diffs)
	}
	// A harmless edit changes nothing.
	same := fact("a", "never push to main branch", vec(d, 0))
	rep, _ = rp.Diff(Change{Upsert: []*Item{same}})
	if rep.LostMemories != 0 || rep.Score != 1 {
		t.Fatalf("harmless edit lost recalls: %+v", rep)
	}
	// Removing it is a loss with reason removed.
	rep, _ = rp.Diff(Change{Remove: []string{"fact:a"}})
	if rep.LostMemories != 1 || rep.Diffs[0].Lost[0].Why != "removed" {
		t.Fatalf("remove: %+v", rep)
	}
}

func TestRemapCountsAMergeAsKept(t *testing.T) {
	d := 8
	a := fact("a", "never push to main", vec(d, 0))
	b := fact("b", "pushes need a PR", vec(d, 0, 1))
	base := NewSnapshot([]*Item{a, b}, nil)
	s := sit("s1", "push it", vec(d, 0), map[string]float64{"fact:a": 1})
	rp := NewReplayer(base, []Situation{s})

	merged := fact("b", "never push to main; pushes need a PR", vec(d, 0))
	// Without a remap nothing says B is A, so deleting A is a loss of A.
	rep, _ := rp.Diff(Change{Upsert: []*Item{merged}, Remove: []string{"fact:a"}})
	if rep.LostMemories != 1 {
		t.Fatalf("a removal without a remap must count as a loss of A: %+v", rep)
	}
	rep, _ = rp.Diff(Change{Upsert: []*Item{merged}, Remap: map[string]string{"fact:a": "fact:b"}})
	if rep.LostMemories != 0 || rep.Score != 1 {
		t.Fatalf("remap: %+v", rep)
	}
	// A merge that moves B away loses it even with the remap.
	away := fact("b", "unrelated now", vec(d, 6))
	rep, _ = rp.Diff(Change{Upsert: []*Item{away}, Remap: map[string]string{"fact:a": "fact:b"}})
	if rep.LostMemories != 1 {
		t.Fatalf("remap to a bad target must still lose: %+v", rep)
	}
}

func TestRemapCarriesCues(t *testing.T) {
	d := 8
	a := fact("a", "rule a", vec(d, 0))
	b := fact("b", "rule b", vec(d, 1))
	cue := NewCue(cues.Cue{Target: "fact:a", Kind: cues.Request, Text: "deploy the thing"}, vec(d, 4))
	base := NewSnapshot([]*Item{a, b}, []CueRec{cue})
	s := sit("s1", "deploy the thing now", vec(d, 4), map[string]float64{"fact:a": 1})
	rp := NewReplayer(base, []Situation{s})
	if len(rp.fire[0]) != 1 || rp.fire[0][0].Target != "fact:a" {
		t.Fatalf("the cue must make A fire: %v", rp.fire[0])
	}
	rep, after := rp.Diff(Change{Remap: map[string]string{"fact:a": "fact:b"}})
	if rep.LostMemories != 0 {
		t.Fatalf("cue not carried to B: %+v", rep)
	}
	if len(after.cues["fact:b"]) != 1 || after.Has("fact:a") {
		t.Fatal("overlay must move the cue and drop A")
	}
	if len(base.cues["fact:a"]) != 1 {
		t.Fatal("the base snapshot must not change")
	}
}

func TestNewFalseFiresAreFlaggedWhereNothingUsefulFired(t *testing.T) {
	d := 8
	a := fact("a", "rule a", vec(d, 0))
	base := NewSnapshot([]*Item{a}, nil)
	quiet := sit("quiet", "something about lunch", vec(d, 5), map[string]float64{})
	busy := sit("busy", "about a", vec(d, 0), map[string]float64{"fact:a": 1})
	rp := NewReplayer(base, []Situation{quiet, busy})
	noisy := fact("n", "broad new memory", vec(d, 0, 5))
	rep, _ := rp.Diff(Change{Upsert: []*Item{noisy}})
	if rep.NewFalseFires != 1 || rep.Gained < 2 {
		t.Fatalf("false fires: %+v", rep)
	}
	for _, df := range rep.Diffs {
		if df.ID == "quiet" && len(df.FalseFire) != 1 {
			t.Fatalf("quiet situation must show the false fire: %+v", df)
		}
		if df.ID == "busy" && len(df.FalseFire) != 0 {
			t.Fatalf("a situation with a useful recall is not a false-fire situation: %+v", df)
		}
	}
}

func TestAvoidListMarksFalseFires(t *testing.T) {
	d := 8
	a := fact("a", "rule a", vec(d, 0))
	base := NewSnapshot([]*Item{a}, nil)
	s := sit("s", "about other", vec(d, 5), map[string]float64{"fact:a": 1})
	s.Avoid = []string{"fact:z"}
	rp := NewReplayer(base, []Situation{s})
	z := fact("z", "z", vec(d, 5))
	rep, _ := rp.Diff(Change{Upsert: []*Item{z}})
	if rep.NewFalseFires != 1 {
		t.Fatalf("avoid: %+v", rep)
	}
}

func TestCommitMakesTheOverlayTheBase(t *testing.T) {
	d := 8
	a := fact("a", "rule a", vec(d, 0))
	base := NewSnapshot([]*Item{a}, nil)
	s := sit("s", "q", vec(d, 0), map[string]float64{"fact:a": 1})
	rp := NewReplayer(base, []Situation{s})
	ch := Change{Remove: []string{"fact:a"}}
	rep, after := rp.Diff(ch)
	if rep.LostMemories != 1 {
		t.Fatal("setup")
	}
	rp.Commit(ch, after)
	rep, _ = rp.Diff(Change{})
	if rep.LostMemories != 0 || len(rp.fire[0]) != 0 {
		t.Fatalf("after commit the base has no A: %v", rp.fire[0])
	}
}

func TestIncrementalMatchesFullRescore(t *testing.T) {
	r := rand.New(rand.NewSource(7))
	d := 32
	var items []*Item
	for i := 0; i < 200; i++ {
		v := make([]float32, d)
		for k := range v {
			v[k] = float32(r.NormFloat64())
		}
		items = append(items, fact(string(rune('a'+i%26))+strings.Repeat("x", i/26), "memory text number "+string(rune('a'+i%26)), Unit(v)))
	}
	base := NewSnapshot(items, nil)
	var sits []Situation
	for i := 0; i < 60; i++ {
		v := make([]float32, d)
		for k := range v {
			v[k] = float32(r.NormFloat64())
		}
		// Make each situation near one memory so something fires.
		near := items[r.Intn(len(items))].Parts[0].Vec
		for k := range v {
			v[k] = near[k]*3 + v[k]*0.2
		}
		sits = append(sits, Situation{ID: string(rune('A' + i)), Text: "q", Vec: v, Expect: map[string]float64{}})
	}
	rp := NewReplayer(base, sits)
	ch := Change{Remove: []string{items[3].Target, items[9].Target},
		Upsert: []*Item{fact("new", "a brand new memory", items[5].Parts[0].Vec)}}
	_, after := rp.Diff(ch)
	full := NewReplayer(after, sits)
	rp.Commit(ch, after)
	for i := range sits {
		a, b := rp.fire[i], full.fire[i]
		if len(a) != len(b) {
			t.Fatalf("situation %d: incremental %v, full %v", i, a, b)
		}
		for k := range a {
			if a[k].Target != b[k].Target || math.Abs(a[k].Score-b[k].Score) > 1e-9 {
				t.Fatalf("situation %d: incremental %v, full %v", i, a, b)
			}
		}
	}
}

func TestThousandSituationsAreFast(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	d := 768
	rv := func() []float32 {
		v := make([]float32, d)
		for k := range v {
			v[k] = float32(r.NormFloat64())
		}
		return Unit(v)
	}
	var items []*Item
	for i := 0; i < 1500; i++ {
		items = append(items, fact(strings.Repeat("m", 1)+time.Duration(i).String(), "text "+time.Duration(i).String(), rv()))
	}
	var sits []Situation
	for i := 0; i < 1000; i++ {
		sits = append(sits, Situation{ID: time.Duration(i).String(), Text: "query words here", Vec: rv(), Expect: map[string]float64{}})
	}
	start := time.Now()
	rp := NewReplayer(NewSnapshot(items, nil), sits)
	build := time.Since(start)
	start = time.Now()
	rp.Diff(Change{Upsert: []*Item{fact("x", "new", rv())}, Remove: []string{items[0].Target}})
	diff := time.Since(start)
	t.Logf("score 1000 situations x 1500 memories: %v; diff of one change: %v", build, diff)
	if build > 8*time.Second || diff > time.Second {
		t.Fatalf("too slow: %v / %v", build, diff)
	}
}

// --- corpus ---

func openStore(t *testing.T, lim Limits) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), ".grimoire"), lim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestCleanScrubsAndBounds(t *testing.T) {
	got := Clean("  use   key sk-ant-api03-AbCdEf0123456789AbCdEf0123456789 now ")
	if strings.Contains(got, "AbCdEf") || !strings.Contains(got, "…") {
		t.Fatalf("token kept: %q", got)
	}
	if got := Clean("internationalization-compatibility-infrastructure"); !strings.Contains(got, "internationalization") {
		t.Fatalf("a long plain word is not a secret: %q", got)
	}
	long := strings.Repeat("word ", 300)
	if n := len([]rune(Clean(long))); n > MaxText {
		t.Fatalf("not bounded: %d", n)
	}
}

func TestNoteDedupesAndResolvesOutcomes(t *testing.T) {
	st := openStore(t, Limits{})
	t0 := time.Now().Add(-time.Hour)
	for i := 0; i < 3; i++ {
		if err := st.Note("s1", "push the branch", "prompt", 0.5, 5, []Fire{{"fact:a", 0.8}, {"fact:b", 0.6}}, t0.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	st.Note("s1", "quiet request", "prompt", 0.5, 5, nil, t0)
	stats, _ := st.Stats()
	if stats.Live != 2 {
		t.Fatalf("same text and stage is one situation: %+v", stats)
	}
	n, err := st.Resolve([]Outcome{
		{Session: "s1", Target: "fact:a", Stage: "prompt", Outcome: "cited", TS: t0.Unix()},
		{Session: "s1", Target: "fact:b", Stage: "prompt", Outcome: "ignored", TS: t0.Unix()},
	}, time.Now())
	if err != nil || n == 0 {
		t.Fatalf("resolve: %d %v", n, err)
	}
	sits, _ := st.Situations(nil, "")
	var push Situation
	for _, s := range sits {
		if s.Text == "push the branch" {
			push = s
		}
	}
	if push.Expect["fact:a"] != 1 || push.Expect["fact:b"] != 0 || push.N != 3 {
		t.Fatalf("expectations: %+v", push)
	}
	if got := push.Useful(); len(got) != 1 || got[0] != "fact:a" {
		t.Fatalf("useful: %v", got)
	}
}

func TestRetentionAndSizeBounds(t *testing.T) {
	st := openStore(t, Limits{MaxLive: 10, MaxSeed: 5, Retention: 24 * time.Hour})
	now := time.Now()
	st.Note("s", "ancient", "prompt", 0.5, 5, []Fire{{"fact:a", 1}}, now.Add(-48*time.Hour))
	for i := 0; i < 30; i++ {
		st.last = time.Time{} // let the prune run
		st.Note("s", "request number "+time.Duration(i).String(), "prompt", 0.5, 5, nil, now.Add(time.Duration(i)*time.Second))
	}
	var seeds []Seed
	for i := 0; i < 12; i++ {
		seeds = append(seeds, Seed{Text: "seed " + time.Duration(i).String(), Expect: []string{"fact:z"}})
	}
	if _, err := st.AddSeeds(seeds, now); err != nil {
		t.Fatal(err)
	}
	stats, _ := st.Stats()
	if stats.Live > 10 || stats.Seed > 5 {
		t.Fatalf("not bounded: %+v", stats)
	}
	sits, _ := st.Situations(nil, "live")
	for _, s := range sits {
		if s.Text == "ancient" {
			t.Fatal("old live situation survived retention")
		}
	}
}

func TestSeedsAreIdempotentAndRemapFollowsAMerge(t *testing.T) {
	st := openStore(t, Limits{})
	seeds := []Seed{{Text: "push it", Expect: []string{"fact:a"}, Avoid: []string{"fact:q"}}}
	if n, _ := st.AddSeeds(seeds, time.Now()); n != 1 {
		t.Fatal("first add")
	}
	if n, _ := st.AddSeeds(seeds, time.Now()); n != 0 {
		t.Fatal("second add must not duplicate")
	}
	if err := st.Remap("fact:a", "fact:b"); err != nil {
		t.Fatal(err)
	}
	sits, _ := st.Situations(nil, "seed")
	if len(sits) != 1 || sits[0].Expect["fact:b"] != 1 || len(sits[0].Expect) != 1 {
		t.Fatalf("remap: %+v", sits)
	}
}

type fakeEmb struct{ calls int }

func (f *fakeEmb) Embed(texts []string) [][]float32 {
	f.calls += len(texts)
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{1, float32(i), 0}
	}
	return out
}
func (f *fakeEmb) Signature() string { return "fake" }

func TestSituationVectorsAreCached(t *testing.T) {
	st := openStore(t, Limits{})
	st.Note("s", "one", "prompt", 0.5, 5, nil, time.Now())
	st.Note("s", "two", "prompt", 0.5, 5, nil, time.Now())
	e := &fakeEmb{}
	sits, _ := st.Situations(e, "")
	if len(sits) != 2 || e.calls != 2 || len(sits[0].Vec) != 3 {
		t.Fatalf("first load: %d calls", e.calls)
	}
	sits, _ = st.Situations(e, "")
	if e.calls != 2 || len(sits[0].Vec) != 3 {
		t.Fatalf("second load must come from the cache: %d calls", e.calls)
	}
}
