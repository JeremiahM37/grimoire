package impact

import (
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

func sess(origin, cwd string, prompts ...string) transcript.Session {
	s := transcript.Session{Agent: "claude-code", Origin: origin, Cwd: cwd, Start: time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)}
	for _, p := range prompts {
		s.Turns = append(s.Turns, transcript.Turn{Role: transcript.RoleUser, Text: p, At: s.Start})
	}
	return s
}

func TestInteractiveFilter(t *testing.T) {
	ss := []transcript.Session{
		sess("cli", "/home/admin/projects/lectern", "please fix the failing build now", "<task-notification> background task finished with output", "[SYSTEM NOTIFICATION] monitor event fired today", "  <system-reminder> the date changed"),
		sess("sdk-cli", "/home/admin/projects/lectern", "headless prompt with enough words"),
		sess("cli", "/tmp/claude-1000/clean/run1", "benchmark prompt with enough words", "second benchmark prompt here"),
		sess("cli", "/home/admin/projects/agentdeck/.agentdeck-worktrees/task15-a1", "worker prompt with enough words"),
		sess("cli", "/home/admin/agentdeck-scratch/zz-readtest-20261008-42WlMb", "probe prompt with enough words"),
		sess("exec", "/home/admin", "codex exec prompt with enough words"),
		sess("", "/home/admin", "an agent with no origin recorded", "[Request interrupted by user for tool use]"),
	}
	got, ex := PromptsOfInteractive(ss, true)
	if len(got) != 2 || got[0].Text != "please fix the failing build now" || ex.PromptsKept != 2 {
		t.Fatalf("kept %+v / %+v", got, ex)
	}
	if ex.SessionsSeen != 7 || ex.SessionsKept != 2 {
		t.Errorf("sessions %+v", ex)
	}
	if ex.SessionsExcluded[RuleHeadless] != 2 || ex.SessionsExcluded[RuleBenchmark] != 1 || ex.SessionsExcluded[RuleAutomation] != 2 {
		t.Errorf("session rules %+v", ex.SessionsExcluded)
	}
	if ex.PromptsInDropped[RuleBenchmark] != 2 || ex.PromptsExcluded[RuleAutomated] != 4 {
		t.Errorf("prompt rules %+v / %+v", ex.PromptsInDropped, ex.PromptsExcluded)
	}
	// Off: everything counts.
	all, ex2 := PromptsOfInteractive(ss, false)
	if len(all) != ex2.PromptsSeen || len(all) != 12 || len(ex2.SessionsExcluded) != 0 {
		t.Errorf("unfiltered %d %+v", len(all), ex2)
	}
}

func TestWilson(t *testing.T) {
	lo, hi := wilson(5, 100)
	if lo < 0.02 || lo > 0.03 || hi < 0.10 || hi > 0.12 {
		t.Errorf("wilson %v %v", lo, hi)
	}
	if lo, hi := wilson(0, 0); lo != 0 || hi != 0 {
		t.Error("empty")
	}
}

func TestDedupePrompts(t *testing.T) {
	at := time.Date(2026, 9, 30, 9, 0, 0, 0, time.UTC)
	ps := []Prompt{{At: at, Agent: "a", Text: "same thing typed once"}, {At: at, Agent: "a", Text: "same thing typed once"},
		{At: at.Add(time.Minute), Agent: "a", Text: "same thing typed once"}, {At: at, Agent: "b", Text: "same thing typed once"}}
	got, n := DedupePrompts(ps)
	if len(got) != 3 || n != 1 {
		t.Errorf("%d %d", len(got), n)
	}
}
