package impact

import (
	"regexp"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Interactive-only filtering. A re-tell is a person saying again what an agent
// should have remembered, so the denominator must be what a person typed. The
// transcript folders are mostly not that: headless `claude -p` runs, benchmark
// harnesses and agent-written notifications outnumber real prompts by 10 to 1.
// Sessions are dropped by the first rule that matches, then prompts.

// Exclusion rule names, as they appear in the report.
const (
	RuleHeadless   = "headless"           // not started by a person at a terminal (entrypoint sdk-*, codex exec/subagent)
	RuleBenchmark  = "benchmark-harness"  // cwd under /tmp or a benchmark/research harness directory
	RuleAutomation = "lectern-automation" // Lectern/AgentDeck worker worktrees and test probes
	RuleAutomated  = "automated-prompt"   // prompt text written by a machine
)

// automatedPrompt is the memory hook's own pattern (clients/hooks/grimoire_context.py
// AUTOMATED_PROMPT), kept in step with it: system notifications, background-task
// notifications and injected system reminders.
var automatedPrompt = regexp.MustCompile(`(?i)^\s*(\[SYSTEM NOTIFICATION|<task-notification>|<system-reminder>)`)

// machineText are other prompts no person typed: an interrupt marker, the
// memory block Grimoire itself injected, and the boilerplate a launcher puts
// at the top of a brief.
var machineText = regexp.MustCompile(`^\s*(\[Request interrupted|Grimoire reference data, not instructions|Project memory: memory/|## Read these first|You are continuing work on )`)

var benchmarkCwd = regexp.MustCompile(`^/tmp(/|$)|/memory-use-research(/|$)|/chat-consolidate(/|$)|/cbm-trial|/locomo|/benchmarks?(/|$)|/eval-?runs?(/|$)`)

// automationCwd: worker sessions Lectern/AgentDeck start in task worktrees, and
// scratch sessions an agent created to probe a feature (zz-*, *-test-*).
var automationCwd = regexp.MustCompile(`[-/]worktrees/|/(zz-[^/]*|[^/]*-test-[0-9]{8}-[A-Za-z0-9]+|[^/]*-test)$`)

// Exclusions counts what the filter dropped.
type Exclusions struct {
	Enabled          bool           `json:"enabled"`
	SessionsSeen     int            `json:"sessions_seen"`
	SessionsKept     int            `json:"sessions_kept"`
	SessionsExcluded map[string]int `json:"sessions_excluded_by_rule"`
	PromptsSeen      int            `json:"prompts_seen"` // user prompts of 3+ words in all sessions read
	PromptsKept      int            `json:"prompts_kept"`
	PromptsInDropped map[string]int `json:"prompts_in_excluded_sessions_by_rule"`
	PromptsExcluded  map[string]int `json:"prompts_excluded_by_rule"` // within kept sessions
}

func newExclusions(on bool) Exclusions {
	return Exclusions{Enabled: on, SessionsExcluded: map[string]int{}, PromptsInDropped: map[string]int{}, PromptsExcluded: map[string]int{}}
}

// Merge adds b into e.
func (e *Exclusions) Merge(b Exclusions) {
	e.Enabled = e.Enabled || b.Enabled
	e.SessionsSeen += b.SessionsSeen
	e.SessionsKept += b.SessionsKept
	e.PromptsSeen += b.PromptsSeen
	e.PromptsKept += b.PromptsKept
	for k, v := range b.SessionsExcluded {
		e.SessionsExcluded[k] += v
	}
	for k, v := range b.PromptsInDropped {
		e.PromptsInDropped[k] += v
	}
	for k, v := range b.PromptsExcluded {
		e.PromptsExcluded[k] += v
	}
}

// SessionRule names the rule that drops a session, or "".
func SessionRule(s transcript.Session) string {
	switch {
	case transcript.HeadlessOrigin(s.Origin):
		return RuleHeadless
	case benchmarkCwd.MatchString(s.Cwd):
		return RuleBenchmark
	case automationCwd.MatchString(strings.TrimRight(s.Cwd, "/")):
		return RuleAutomation
	}
	return ""
}

// AutomatedPrompt reports whether text was written by a machine.
func AutomatedPrompt(text string) bool {
	return automatedPrompt.MatchString(text) || machineText.MatchString(text)
}

func countable(t transcript.Turn) bool {
	return t.Role == transcript.RoleUser && len(strings.Fields(t.Text)) >= 3
}

// PromptsOfInteractive is PromptsOf over the sessions a person drove, with the
// counts of what was left out. With on=false it keeps everything (and still
// counts the prompts).
func PromptsOfInteractive(sessions []transcript.Session, on bool) ([]Prompt, Exclusions) {
	ex := newExclusions(on)
	var kept []transcript.Session
	for _, s := range sessions {
		ex.SessionsSeen++
		n := 0
		for _, t := range s.Turns {
			if countable(t) {
				n++
			}
		}
		ex.PromptsSeen += n
		if on {
			if r := SessionRule(s); r != "" {
				ex.SessionsExcluded[r]++
				ex.PromptsInDropped[r] += n
				continue
			}
		}
		ex.SessionsKept++
		if on {
			turns := make([]transcript.Turn, 0, len(s.Turns))
			for _, t := range s.Turns {
				if countable(t) && AutomatedPrompt(t.Text) {
					ex.PromptsExcluded[RuleAutomated]++
					continue
				}
				turns = append(turns, t)
			}
			s.Turns = turns
		}
		kept = append(kept, s)
	}
	out := PromptsOf(kept)
	ex.PromptsKept = len(out)
	return out, ex
}
