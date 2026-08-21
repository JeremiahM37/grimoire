package eval

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// A retriever that returns whatever it is told to, so scoring can be tested
// without an index.
type fake struct {
	byQuery map[string][]Passage
}

func (f *fake) Rank(q string, k int) ([]Passage, error) {
	out := f.byQuery[q]
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

func set(qs ...Question) Set {
	return Set{Generator: "lexical", Questions: qs}
}

func TestScoringIsExactMembershipNotAJudgement(t *testing.T) {
	s := set(
		Question{Q: "q1", Path: "a.md", Chunk: 0},
		Question{Q: "q2", Path: "b.md", Chunk: 1},
	)
	r := &fake{byQuery: map[string][]Passage{
		"q1": {{Path: "a.md", Chunk: 0}},                           // hit at rank 1
		"q2": {{Path: "z.md", Chunk: 0}, {Path: "b.md", Chunk: 1}}, // hit at rank 2
	}}
	res, err := Score(s, r, 8, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if res.RecallAtK != 1 {
		t.Errorf("recall = %v, want 1", res.RecallAtK)
	}
	// 1/1 and 1/2, averaged.
	if res.MRR != 0.75 {
		t.Errorf("MRR = %v, want 0.75", res.MRR)
	}
	if len(res.Failures) != 0 {
		t.Errorf("failures on a perfect run: %v", res.Failures)
	}
}

func TestTheRightNoteAndTheWrongPassageIsAMiss(t *testing.T) {
	// The distinction the whole set is built around. "Somewhere in this note"
	// is a far easier target than the passage the question came from, and
	// scoring it as a hit would make chunking changes invisible.
	s := set(Question{Q: "q1", Path: "a.md", Chunk: 7})
	r := &fake{byQuery: map[string][]Passage{
		"q1": {{Path: "a.md", Chunk: 0}, {Path: "a.md", Chunk: 1}},
	}}
	res, _ := Score(s, r, 8, Config{})
	if res.RecallAtK != 0 {
		t.Errorf("recall = %v; the wrong passage of the right note counted as a hit",
			res.RecallAtK)
	}
	// …and note-recall says so, which is what turns "it got worse" into
	// "chunking got worse".
	if res.NoteRecallAtK != 1 {
		t.Errorf("note recall = %v, want 1", res.NoteRecallAtK)
	}
	if len(res.Failures) != 1 || !res.Failures[0].NoteHit {
		t.Errorf("failure does not report the near miss: %+v", res.Failures)
	}
	if res.Failures[0].BestRank != 0 {
		t.Errorf("best rank = %d, want the note's rank", res.Failures[0].BestRank)
	}
}

func TestKBoundsTheSearchDepth(t *testing.T) {
	s := set(Question{Q: "q1", Path: "a.md", Chunk: 0})
	got := []Passage{}
	for i := 0; i < 20; i++ {
		got = append(got, Passage{Path: "filler.md", Chunk: i})
	}
	got = append(got, Passage{Path: "a.md", Chunk: 0}) // rank 21
	r := &fake{byQuery: map[string][]Passage{"q1": got}}

	if res, _ := Score(s, r, 8, Config{}); res.RecallAtK != 0 {
		t.Errorf("recall@8 = %v for a hit at rank 21", res.RecallAtK)
	}
	if res, _ := Score(s, r, 25, Config{}); res.RecallAtK != 1 {
		t.Errorf("recall@25 = %v for a hit at rank 21", res.RecallAtK)
	}
}

func TestAFailureReportsWhatCameBackInstead(t *testing.T) {
	// "It got worse" has to turn into a list of things to look at.
	s := set(Question{Q: "where do we deploy?", Path: "a.md", Chunk: 0})
	r := &fake{byQuery: map[string][]Passage{
		"where do we deploy?": {{Path: "x.md", Chunk: 0}, {Path: "y.md", Chunk: 3}},
	}}
	res, _ := Score(s, r, 8, Config{})
	if len(res.Failures) != 1 {
		t.Fatalf("failures = %v", res.Failures)
	}
	f := res.Failures[0]
	if f.Want != "a.md#0" {
		t.Errorf("want = %q", f.Want)
	}
	if len(f.Got) == 0 || f.Got[0] != "x.md#0" {
		t.Errorf("got = %v", f.Got)
	}
	if f.NoteHit || f.BestRank != -1 {
		t.Errorf("a total miss reported a note hit: %+v", f)
	}
}

func TestCompareReportsWhichQuestionsMoved(t *testing.T) {
	// Two runs that score the same can disagree about a third of their
	// questions, and "no change" would be a lie about what happened.
	base := Result{K: 8, Questions: 4, RecallAtK: 0.5, MRR: 0.4,
		Failures: []Failure{{Q: "q1"}, {Q: "q2"}}}
	cur := Result{K: 8, Questions: 4, RecallAtK: 0.5, MRR: 0.4,
		Failures: []Failure{{Q: "q2"}, {Q: "q3"}}}

	c := Compare(base, cur)
	if c.RecallDelta != 0 {
		t.Errorf("recall delta = %v", c.RecallDelta)
	}
	if len(c.Fixed) != 1 || c.Fixed[0] != "q1" {
		t.Errorf("fixed = %v", c.Fixed)
	}
	if len(c.Broken) != 1 || c.Broken[0] != "q3" {
		t.Errorf("broken = %v", c.Broken)
	}
}

func TestCompareFlagsAChangedConfiguration(t *testing.T) {
	base := Result{K: 8, Config: Config{Embedder: "hash", Dim: 256}}
	cur := Result{K: 8, Config: Config{Embedder: "nomic-embed-text", Dim: 768}}
	if Compare(base, cur).SameConfig {
		t.Error("a different embedder was compared as like-for-like")
	}
	// A vault that grew by three notes is still the same configuration.
	grew := Result{K: 8, Config: Config{Embedder: "hash", Dim: 256, Notes: 900}}
	if !Compare(base, grew).SameConfig {
		t.Error("corpus growth was treated as a configuration change")
	}
}

func TestSamplingIsStableAcrossRuns(t *testing.T) {
	// A set that is regenerated must be recognisably the same measurement, or
	// "rebuild and compare" silently compares two different things.
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("note-%03d.md#0", i)
	}
	a := pick(ids, 20)
	b := pick(ids, 20)
	if strings.Join(a, ",") != strings.Join(b, ",") {
		t.Error("two samples of the same corpus differ")
	}
	if len(a) != 20 {
		t.Fatalf("sampled %d", len(a))
	}
}

func TestSamplingDoesNotFollowTheAlphabet(t *testing.T) {
	// Taking every m-th id would sample the vault's filenames rather than its
	// content, so a folder called "aaa-archive" would dominate every set.
	ids := []string{}
	for i := 0; i < 100; i++ {
		ids = append(ids, fmt.Sprintf("aaa-archive/n-%03d.md#0", i))
	}
	for i := 0; i < 100; i++ {
		ids = append(ids, fmt.Sprintf("zzz-current/n-%03d.md#0", i))
	}
	got := pick(ids, 40)
	archive := 0
	for _, id := range got {
		if strings.HasPrefix(id, "aaa-") {
			archive++
		}
	}
	if archive == 0 || archive == len(got) {
		t.Errorf("%d of %d sampled ids came from one folder — the sample is "+
			"following the alphabet", archive, len(got))
	}
}

func TestSetsRoundTrip(t *testing.T) {
	s := Set{Created: "2026-08-21T00:00:00Z", Generator: "llm", Model: "ollama",
		Questions: []Question{{Q: "where?", Path: "a.md", Chunk: 2, Excerpt: "…"}}}
	var buf bytes.Buffer
	if err := WriteSet(&buf, s); err != nil {
		t.Fatal(err)
	}
	back, err := ReadSet(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Generator != "llm" || len(back.Questions) != 1 ||
		back.Questions[0].Chunk != 2 {
		t.Errorf("round trip lost data: %+v", back)
	}
}

func TestAnEmptySetIsAnErrorNotAPerfectScore(t *testing.T) {
	// A file that decoded to zero questions must not read as 100% recall.
	if _, err := ReadSet(strings.NewReader(`{"questions":[]}`)); err == nil {
		t.Error("an empty question set loaded without complaint")
	}
}

// ------------------------------------------------------------- generation

func chunksFor(n int) []Chunk {
	out := make([]Chunk, n)
	for i := range out {
		out[i] = Chunk{
			Path: fmt.Sprintf("note-%02d.md", i), Index: 0,
			Title: fmt.Sprintf("Note %d", i),
			Text: "The kestrel deployment procedure requires rotating the " +
				"ingress certificate before restarting the gateway, because " +
				"the gateway caches the certificate chain at boot and will " +
				"otherwise serve the expired one until somebody notices.",
		}
	}
	return out
}

func TestTheLexicalGeneratorProducesUsableQuestions(t *testing.T) {
	s, err := Generate(chunksFor(20), 10, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Generator != "lexical" {
		t.Errorf("generator = %q", s.Generator)
	}
	if len(s.Questions) != 10 {
		t.Fatalf("got %d questions", len(s.Questions))
	}
	for _, q := range s.Questions {
		if q.Q == "" || q.Path == "" {
			t.Errorf("empty question: %+v", q)
		}
		if q.Excerpt == "" {
			t.Errorf("no excerpt kept for %q — a failure would be unreadable", q.Q)
		}
	}
}

func TestShortPassagesAreNotAskedAbout(t *testing.T) {
	// A chunk that is three words of frontmatter has no question in it, and
	// inventing one adds noise to every future comparison.
	short := []Chunk{{Path: "a.md", Text: "# Heading"}, {Path: "b.md", Text: "todo"}}
	if _, err := Generate(short, 5, nil); err == nil {
		t.Error("generated questions from passages with nothing in them")
	}
}

func TestGeneratingFromAnEmptyVaultSaysSo(t *testing.T) {
	if _, err := Generate(nil, 10, nil); err == nil {
		t.Error("generated a question set from no passages")
	}
}

// stubWriter stands in for a model.
type stubWriter struct {
	reply string
	fail  bool
	calls int
}

func (s *stubWriter) Name() string { return "stub" }
func (s *stubWriter) WriteQuestion(title, chunk string) (string, error) {
	s.calls++
	if s.fail {
		return "", fmt.Errorf("model unavailable")
	}
	return s.reply, nil
}

func TestTheLLMGeneratorIsRecordedInTheSet(t *testing.T) {
	// The two generators measure different things, so their numbers must never
	// be compared — which is only possible if the set says which wrote it.
	w := &stubWriter{reply: "Why must the certificate be rotated first?"}
	s, err := Generate(chunksFor(5), 3, w)
	if err != nil {
		t.Fatal(err)
	}
	if s.Generator != "llm" || s.Model != "stub" {
		t.Errorf("set = %+v", s)
	}
	if len(s.Questions) != 3 {
		t.Errorf("got %d questions from %d calls", len(s.Questions), w.calls)
	}
}

func TestOnePassageFailingDoesNotAbandonTheSet(t *testing.T) {
	// Building a set takes minutes; one timeout must not throw it away.
	w := &stubWriter{fail: true}
	if _, err := Generate(chunksFor(5), 3, w); err == nil {
		t.Error("a set was produced from a writer that failed every call")
	}
	if w.calls < 3 {
		t.Errorf("gave up after %d calls", w.calls)
	}
}

func TestAModelThatAnswersInsteadOfAskingIsRejected(t *testing.T) {
	for _, reply := range []string{
		"SKIP",
		"The certificate is rotated before the gateway restarts.", // no question mark
		"",
		strings.Repeat("why ", 200) + "?", // runaway
	} {
		if got := cleanQuestion(reply); got != "" {
			t.Errorf("cleanQuestion(%.30q) = %q, want it rejected", reply, got)
		}
	}
}

func TestAModelsFormattingIsCleanedUp(t *testing.T) {
	for reply, want := range map[string]string{
		`"Why rotate the certificate first?"`:     "Why rotate the certificate first?",
		"Question: Why rotate the certificate?":   "Why rotate the certificate?",
		"thinking...\n\nWhy rotate it?":           "Why rotate it?",
		"  Why rotate the certificate first?  \n": "Why rotate the certificate first?",
	} {
		if got := cleanQuestion(reply); got != want {
			t.Errorf("cleanQuestion(%q) = %q, want %q", reply, got, want)
		}
	}
}

func TestTheLexicalQuestionUsesDistinctiveWords(t *testing.T) {
	q := LexicalQuestion("Deploy runbook",
		"The kestrel gateway must have its ingress certificate rotated before "+
			"the gateway is restarted, and the gateway caches the chain.")
	if !strings.Contains(q, "Deploy runbook") {
		t.Errorf("question dropped the note title: %q", q)
	}
	for _, want := range []string{"kestrel", "certificate"} {
		if !strings.Contains(strings.ToLower(q), want) {
			t.Errorf("question %q is missing the distinctive term %q", q, want)
		}
	}
	// "gateway" appears three times, so it discriminates less than "kestrel"
	// — the ranking should prefer the rarer word when both cannot fit.
	if strings.Count(strings.ToLower(q), "gateway") > 1 {
		t.Errorf("question repeats a common term: %q", q)
	}
}
