package impact

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var memText = "Deploy kestrel always to the staging cluster first, never straight to production, because the migration job runs on staging"

func pr(day int, text string) Prompt {
	return Prompt{At: time.Date(2026, 9, 28, 10, 0, 0, 0, time.UTC).AddDate(0, 0, day), Agent: "claude-code", Text: text}
}

func TestRetellsWeeklyRateCacheAndSampling(t *testing.T) {
	notes := []NoteText{{Name: "reference_kestrel.md", Text: memText, At: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)},
		{Name: "other.md", Text: "completely unrelated grocery list banana apple"}}
	prompts := []Prompt{
		pr(0, "deploy kestrel to the staging cluster first, never straight to production please"), // week 40: retell
		pr(1, "write a haiku about the sea and the moon"),
		pr(7, "deploy kestrel staging cluster first not production, I told you the migration job runs on staging"), // week 41
		pr(8, "how does the kestrel migration job work on staging"),                                                // same topic, a question
		pr(9, "add a unit test for the parser module"),
	}
	calls := 0
	judge := func(mem, prompt string) (bool, error) {
		calls++
		return strings.Contains(prompt, "never straight") || strings.Contains(prompt, "I told you"), nil
	}
	cache := filepath.Join(t.TempDir(), "retell-cache.jsonl")
	cut := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	o := RetellOptions{Judge: judge, JudgeName: "test-judge", CachePath: cache, Cutoff: cut, Workers: 1}
	r := Retells(prompts, notes, o)
	if r.Judge != "test-judge" || len(r.Weeks) != 2 || r.Sampled {
		t.Fatalf("%+v", r)
	}
	if r.Weeks[0].Yes != 1 || r.Weeks[0].Prompts != 2 || r.Weeks[0].PerHundred != 50 {
		t.Errorf("week 1: %+v", r.Weeks[0])
	}
	if r.Weeks[1].Yes != 1 || r.Weeks[1].Prompts != 3 || r.Weeks[1].HeldThen != 1 {
		t.Errorf("week 2: %+v", r.Weeks[1])
	}
	if !r.Weeks[1].AfterCutoff && r.After.Weeks == 0 && r.Before.Weeks != 2 {
		t.Errorf("phases %+v %+v", r.Before, r.After)
	}
	first := calls
	if first == 0 || r.JudgeCalls != first {
		t.Fatalf("calls %d / %d", first, r.JudgeCalls)
	}
	// A second run reads the cache and calls nothing.
	r2 := Retells(prompts, notes, o)
	if calls != first || r2.JudgeCalls != 0 || r2.CacheHits != first || r2.Weeks[0].Yes != 1 {
		t.Errorf("second run: calls %d, %+v", calls, r2)
	}
	// Over budget: a deterministic sample, flagged, scaled.
	o.CachePath, o.Budget = "", 1
	r3 := Retells(prompts, notes, o)
	if !r3.Sampled || r3.JudgeCalls != 1 {
		t.Errorf("sampled: %+v", r3)
	}
	if !strings.Contains(RetellText(r3), "SAMPLED") {
		t.Error("text must say it sampled")
	}
}

func TestRetellsStrictFallbackAndJudgeErrors(t *testing.T) {
	notes := []NoteText{{Name: "a.md", Text: memText}}
	same := pr(0, "deploy kestrel staging cluster first never production migration job runs staging")
	diff := pr(1, "kestrel staging cluster is down, what do we do")
	r := Retells([]Prompt{same, diff}, notes, RetellOptions{})
	if r.Judge != "strict" || r.JudgeCalls != 0 || sumYes(r) != 1 {
		t.Errorf("%+v", r)
	}
	bad := func(string, string) (bool, error) { return false, errors.New("down") }
	r = Retells([]Prompt{same}, notes, RetellOptions{Judge: bad, JudgeName: "j", Workers: 1})
	if r.Errors != 1 || sumYes(r) != 0 || r.Weeks[0].Evaluated != 0 {
		t.Errorf("a failing judge must count nothing: %+v", r)
	}
}

func sumYes(r RetellReport) int {
	n := 0
	for _, w := range r.Weeks {
		n += w.Yes
	}
	return n
}
