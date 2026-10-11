package distill

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// now is fixed so "old" and "young" mean the same thing on every machine.
var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.Local)

func stampDaysAgo(days int) string {
	return now.AddDate(0, 0, -days).Format(memory.StampFormat)
}

// mk builds a fact that passes every check unless a test changes it. It is a
// person's assertion, old enough, with no lifecycle markers. The id is minted
// from its content so the fact is not mistaken for a hand edit.
func mk(note, text string, mutate func(*memory.Entry)) Candidate {
	e := memory.Entry{
		Text:     text,
		Stamp:    stampDaysAgo(60),
		Agent:    "claude-code",
		Category: "fact",
		Human:    true,
	}
	if mutate != nil {
		mutate(&e)
	}
	e.ID = memory.DeriveID(e.Stamp, e.Agent, e.Text)
	return Candidate{Entry: e, Note: note, Uses: 5}
}

func opts(minUses int) Options {
	return Options{Now: now, MinAgeDays: DefaultMinAgeDays, MinUses: minUses}
}

func TestSelectReasons(t *testing.T) {
	cases := []struct {
		name   string
		cand   Candidate
		minUse int
		reason string
	}{
		{"sealed", mk("memory/a.md", memory.SealedPlaceholder, func(e *memory.Entry) { e.Visibility = memory.VisSealed }), 0, ReasonSealed},
		{"private", mk("memory/a.md", "the user owns a cat", func(e *memory.Entry) { e.Visibility = memory.VisPrivate }), 0, ReasonPrivate},
		{"sensitive", mk("memory/a.md", "the user banks at a bank", func(e *memory.Entry) { e.Visibility = memory.VisSensitive }), 0, ReasonSensitive},
		{"superseded", mk("memory/a.md", "the user prefers spaces", func(e *memory.Entry) { e.SupersededBy = "other" }), 0, ReasonSuperseded},
		{"expired", mk("memory/a.md", "the box has a fixed ip", func(e *memory.Entry) {
			e.Expires = now.Add(-time.Hour).Format(time.RFC3339)
		}), 0, ReasonExpired},
		{"validity ended", mk("memory/a.md", "the office lease runs to march", func(e *memory.Entry) {
			e.ValidTo = memory.FormatValidity(now.Add(-24 * time.Hour))
		}), 0, ReasonValidityEnded},
		{"image caption", mk("memory/a.md", "a photo of a rack", func(e *memory.Entry) { e.Image = strings.Repeat("a", 64) }), 0, ReasonImage},
		{"challenger", mk("memory/a.md", "the user prefers tabs", func(e *memory.Entry) { e.Challenges = "someid" }), 0, ReasonOpenChallenge},
		{"untrusted origin", mk("memory/a.md", "the release date is friday", func(e *memory.Entry) {
			e.Human = false
			e.Origin = trust.Connector("slack", "ws")
			e.Evidence = "slack/msg-1"
			e.Verification = memory.VerifiedEvidence
			e.Category = "observation"
		}), 0, ReasonUntrusted},
		{"imported", mk("memory/a.md", "the user likes rust", func(e *memory.Entry) {
			e.Human = false
			e.Origin = memory.ImportOriginPrefix + "mem0"
		}), 0, ReasonImported},
		{"inferred", mk("memory/a.md", "the user likes rust", func(e *memory.Entry) {
			e.Human = false
			e.Category = ""
			e.Evidence = ""
			e.Verification = ""
		}), 0, ReasonInferred},
		{"unverified evidence", mk("memory/a.md", "the nas has eight disks", func(e *memory.Entry) {
			e.Human = false
			e.Evidence = "notes/nas.md"
			e.Verification = memory.UnverifiedEvidence
			e.Category = "observation"
		}), 0, ReasonUnverified},
		{"agent without verification", mk("memory/a.md", "the nas has eight disks", func(e *memory.Entry) {
			e.Human = false
			e.Evidence = "notes/nas.md"
			e.Verification = ""
			e.Category = "observation"
		}), 0, ReasonAgentUnverified},
		{"too young", mk("memory/a.md", "the user prefers tabs", func(e *memory.Entry) {
			e.Stamp = stampDaysAgo(5)
		}), 0, ReasonTooYoung},
		{"no timestamp", mk("memory/a.md", "the user prefers tabs", func(e *memory.Entry) {
			e.Stamp = ""
		}), 0, ReasonNoTimestamp},
		{"too few uses", mk("memory/a.md", "the user prefers tabs", nil), 3, ReasonTooFewUses},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := tc.cand
			if tc.name == "too few uses" {
				c.Uses = 1
			}
			res, err := Select([]Candidate{c}, Options{Now: now, MinAgeDays: DefaultMinAgeDays, MinUses: tc.minUse})
			if err != nil {
				t.Fatal(err)
			}
			if len(res.Selected) != 0 || len(res.Excluded) != 1 {
				t.Fatalf("selected %d excluded %d, want 0 and 1", len(res.Selected), len(res.Excluded))
			}
			if got := res.Excluded[0].Reason; got != tc.reason {
				t.Errorf("reason = %q, want %q", got, tc.reason)
			}
		})
	}
}

func TestDisputeMarksTargetAndChallenger(t *testing.T) {
	target := mk("memory/a.md", "the user prefers spaces", nil)
	challenger := mk("memory/a.md", "the user prefers tabs", func(e *memory.Entry) {
		e.Human = false
		e.Evidence = "notes/style.md"
		e.Verification = memory.VerifiedEvidence
		e.Category = "observation"
		e.Challenges = target.Entry.ID
	})
	res, err := Select([]Candidate{target, challenger}, opts(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 0 {
		t.Fatalf("a fact in an open dispute was selected: %+v", res.Selected)
	}
	reasons := map[string]string{}
	for _, x := range res.Excluded {
		reasons[x.ID] = x.Reason
	}
	if reasons[target.Entry.ID] != ReasonDisputed {
		t.Errorf("disputed target reason = %q", reasons[target.Entry.ID])
	}
	if reasons[challenger.Entry.ID] != ReasonOpenChallenge {
		t.Errorf("challenger reason = %q", reasons[challenger.Entry.ID])
	}
}

func TestResolvedChallengeDoesNotBlock(t *testing.T) {
	// A challenger that has been superseded no longer contests anything.
	target := mk("memory/a.md", "the user prefers spaces", nil)
	challenger := mk("memory/a.md", "the user prefers tabs", func(e *memory.Entry) {
		e.Challenges = target.Entry.ID
		e.SupersededBy = "later"
	})
	res, err := Select([]Candidate{target, challenger}, opts(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 1 || res.Selected[0].Entry.ID != target.Entry.ID {
		t.Fatalf("a resolved dispute still blocked the target: %+v", res.Excluded)
	}
}

func TestSelectAcceptsHumanAndVerifiedAgent(t *testing.T) {
	human := mk("memory/a.md", "the user prefers tabs", nil)
	agent := mk("memory/b.md", "the nas has eight disks", func(e *memory.Entry) {
		e.Human = false
		e.Evidence = "notes/nas.md"
		e.Verification = memory.VerifiedEvidence
		e.Category = "observation"
	})
	res, err := Select([]Candidate{agent, human}, opts(0))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Selected) != 2 || len(res.Excluded) != 0 {
		t.Fatalf("selected %d excluded %+v", len(res.Selected), res.Excluded)
	}
	// Sorted by note then id, whatever order the input came in.
	if res.Selected[0].Note != "memory/a.md" || res.Selected[1].Note != "memory/b.md" {
		t.Errorf("selection not sorted by note: %q %q", res.Selected[0].Note, res.Selected[1].Note)
	}
}

func TestSelectIsOrderIndependent(t *testing.T) {
	a := mk("memory/a.md", "the user prefers tabs", nil)
	b := mk("memory/b.md", "the user runs a lab", func(e *memory.Entry) { e.Stamp = stampDaysAgo(3) })
	c := mk("memory/c.md", "the box is in the rack", nil)
	r1, _ := Select([]Candidate{a, b, c}, opts(0))
	r2, _ := Select([]Candidate{c, b, a}, opts(0))
	j1, _ := json.Marshal(r1.Excluded)
	j2, _ := json.Marshal(r2.Excluded)
	if !bytes.Equal(j1, j2) || len(r1.Selected) != len(r2.Selected) {
		t.Fatalf("order changed the result:\n%s\n%s", j1, j2)
	}
}

func TestSelectRejectsBadOptions(t *testing.T) {
	if _, err := Select(nil, Options{MinAgeDays: 30}); err == nil {
		t.Error("zero Now accepted")
	}
	if _, err := Select(nil, Options{Now: now, MinAgeDays: -1}); err == nil {
		t.Error("negative age accepted")
	}
}

func TestExamplesFromAttributeAndSlot(t *testing.T) {
	sel, _ := Select([]Candidate{
		mk("memory/a.md", "the user prefers tabs", nil),
		mk("memory/a.md", "deploys go out on thursday afternoons", nil),
	}, opts(0))
	b := Build(sel, nil)
	train := questions(b.Train)
	mustHave(t, train, "Complete this memory entry: user prefers ___")
	mustHave(t, train, "What does your memory say about deploys go out on thursday afternoons?")
	evalQ := questions(b.Eval)
	mustHave(t, evalQ, "Fill in from memory: user prefers ?")
	mustHave(t, evalQ, "Recall what is on file about user.")
}

func TestHeldOutHasNoTemplateOverlap(t *testing.T) {
	dir := t.TempDir()
	m, err := Run(facts(), opts(0), dir)
	if err != nil {
		t.Fatal(err)
	}
	trainT := map[string]bool{}
	for _, id := range m.Templates.Train {
		trainT[id] = true
	}
	for _, id := range m.Templates.Eval {
		if trainT[id] {
			t.Errorf("template %s is used in both splits", id)
		}
	}
	trainQ := map[string]bool{}
	for _, q := range questions(readExamples(t, filepath.Join(dir, TrainFile))) {
		trainQ[q] = true
	}
	for _, q := range questions(readExamples(t, filepath.Join(dir, EvalFile))) {
		if trainQ[q] {
			t.Errorf("held-out question also in training: %q", q)
		}
	}
}

func TestAbstainExamplesPresentAndAbsent(t *testing.T) {
	dir := t.TempDir()
	cands := facts()
	m, err := Run(cands, opts(0), dir)
	if err != nil {
		t.Fatal(err)
	}
	n := m.Counts.Selected
	if m.Counts.TrainAbstain != n || m.Counts.EvalAbstain != n || n == 0 {
		t.Fatalf("abstain counts train=%d eval=%d for %d selected", m.Counts.TrainAbstain, m.Counts.EvalAbstain, n)
	}
	var corpus []string
	for _, c := range cands {
		corpus = append(corpus, strings.ToLower(c.Entry.Text))
	}
	trainSubj := map[string]bool{}
	for _, ex := range readExamples(t, filepath.Join(dir, TrainFile)) {
		if ex.Kind != "abstain" {
			continue
		}
		if ex.Messages[2].Content != AbstainAnswer {
			t.Errorf("abstain answer = %q", ex.Messages[2].Content)
		}
		trainSubj[ex.Messages[1].Content] = true
	}
	for _, ex := range readExamples(t, filepath.Join(dir, EvalFile)) {
		if ex.Kind != "abstain" {
			continue
		}
		if trainSubj[ex.Messages[1].Content] {
			t.Errorf("eval abstain question also in training: %q", ex.Messages[1].Content)
		}
		for _, text := range corpus {
			if strings.Contains(text, strings.ToLower(ex.Messages[1].Content)) {
				t.Errorf("abstain subject is in the store: %q", ex.Messages[1].Content)
			}
		}
	}
}

func TestOutputIsByteIdenticalAcrossRuns(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	if _, err := Run(facts(), opts(0), d1); err != nil {
		t.Fatal(err)
	}
	// Feed the candidates in reverse: input order must not leak into bytes.
	rev := facts()
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	if _, err := Run(rev, opts(0), d2); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{TrainFile, EvalFile, ExclusionsFile, ManifestFile} {
		a, errA := os.ReadFile(filepath.Join(d1, name))
		b, errB := os.ReadFile(filepath.Join(d2, name))
		if errA != nil || errB != nil {
			t.Fatalf("%s: %v %v", name, errA, errB)
		}
		if !bytes.Equal(a, b) {
			t.Errorf("%s differs between runs", name)
		}
	}
}

func TestPrivateTextNeverReachesOutput(t *testing.T) {
	const secret = "SECRETVALUE-7731"
	cands := append(facts(),
		mk("memory/p.md", "the vpn password is "+secret, func(e *memory.Entry) { e.Visibility = memory.VisPrivate }),
		mk("memory/p.md", "the card number ends "+secret, func(e *memory.Entry) { e.Visibility = memory.VisSealed }))
	dir := t.TempDir()
	if _, err := Run(cands, opts(0), dir); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	for _, de := range entries {
		raw, _ := os.ReadFile(filepath.Join(dir, de.Name()))
		if bytes.Contains(raw, []byte(secret)) {
			t.Errorf("%s contains a hidden fact", de.Name())
		}
	}
}

func TestManifestCountsAddUp(t *testing.T) {
	dir := t.TempDir()
	m, err := Run(append(facts(), mk("memory/x.md", "young", func(e *memory.Entry) { e.Stamp = stampDaysAgo(1) })), opts(0), dir)
	if err != nil {
		t.Fatal(err)
	}
	sum := 0
	for _, n := range m.Counts.ExcludedByReason {
		sum += n
	}
	if sum != m.Counts.Excluded || m.Counts.Selected+m.Counts.Excluded != m.Counts.Candidates {
		t.Errorf("counts do not add up: %+v", m.Counts)
	}
	if m.Counts.ExcludedByReason[ReasonTooYoung] != 1 {
		t.Errorf("too_young = %d", m.Counts.ExcludedByReason[ReasonTooYoung])
	}
}

func TestEmptySelectionStillWritesFiles(t *testing.T) {
	dir := t.TempDir()
	m, err := Run(nil, opts(0), dir)
	if err != nil {
		t.Fatal(err)
	}
	if m.Counts.ExcludedByReason == nil || m.Counts.Selected != 0 {
		t.Errorf("empty run: %+v", m.Counts)
	}
	for _, name := range []string{TrainFile, EvalFile, ExclusionsFile, ManifestFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
}

func TestStaleFactShowsInManifestDiff(t *testing.T) {
	d1, d2 := t.TempDir(), t.TempDir()
	cands := facts()
	m1, err := Run(cands, opts(0), d1)
	if err != nil {
		t.Fatal(err)
	}
	// The fact is later superseded: the next selection must drop it, and the
	// diff is how a reader learns the baked model still believes it.
	cands[0].Entry.SupersededBy = "newer"
	m2, err := Run(cands, opts(0), d2)
	if err != nil {
		t.Fatal(err)
	}
	added, removed := FactDiff(m1, m2)
	if len(added) != 0 || len(removed) != 1 || removed[0].ID != cands[0].Entry.ID {
		t.Fatalf("diff added=%v removed=%v", added, removed)
	}
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := Run(facts(), opts(0), dir); err != nil {
		t.Fatal(err)
	}
	m, err := ReadManifest(filepath.Join(dir, ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if m.Date != "2026-10-10" || m.Format != ManifestFormat {
		t.Errorf("manifest header = %+v", m)
	}
	if _, err := ReadManifest(filepath.Join(dir, TrainFile)); err == nil {
		t.Error("a non-manifest file was accepted")
	}
}

// facts is a small store with enough kinds of fact to exercise every path.
func facts() []Candidate {
	return []Candidate{
		mk("memory/prefs.md", "the user prefers tabs", nil),
		mk("memory/ops.md", "deploys go out on thursday afternoons", nil),
		mk("memory/lab.md", "the lab rack has four nodes", nil),
	}
}

func questions(exs []Example) []string {
	var out []string
	for _, ex := range exs {
		out = append(out, ex.Messages[1].Content)
	}
	return out
}

func mustHave(t *testing.T, got []string, want string) {
	t.Helper()
	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Errorf("no example asks %q; have %q", want, got)
}

func readExamples(t *testing.T, path string) []Example {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []Example
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var ex Example
		if err := json.Unmarshal([]byte(line), &ex); err != nil {
			t.Fatalf("bad JSONL line %q: %v", line, err)
		}
		out = append(out, ex)
	}
	return out
}
