package rulecheck

import (
	"math"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/impact"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Match is one historical tool call a check flagged.
type Match struct {
	Session string    `json:"session"`
	Agent   string    `json:"agent"`
	Cwd     string    `json:"cwd,omitempty"`
	Tool    string    `json:"tool"`
	Target  string    `json:"target"`
	At      time.Time `json:"at"`
	Earlier int       `json:"earlier_calls"`    // calls before it in its session (require_before context)
	Prompt  string    `json:"prompt,omitempty"` // what the user last said before the call, for "unless asked" rules
}

// Backtest is what a check would have done over the history it was run on.
type Backtest struct {
	Scanned   int     `json:"calls_scanned"` // calls in the check's tool and scope
	Actions   int     `json:"actions"`       // calls that were the thing the rule is about
	Matches   int     `json:"matches"`       // calls the check flags
	Sessions  int     `json:"sessions"`      // distinct sessions with a match
	Rate      float64 `json:"match_rate"`    // matches / scanned
	Samples   []Match `json:"-"`             // every match, bounded, for labelling
	Sessions0 int     `json:"-"`
}

// MaxKeptMatches bounds the matches kept per check.
const MaxKeptMatches = 400

// Corpus is the interactive history a backtest runs over, flattened.
type Corpus struct {
	Sessions []transcript.Session
	Calls    int
}

// BuildCorpus dedupes tool calls replayed by resumed sessions (same agent,
// time, tool and target) and drops sessions with no tool calls.
func BuildCorpus(sessions []transcript.Session) Corpus {
	seen := map[string]bool{}
	var out []transcript.Session
	n := 0
	for _, s := range sessions {
		turns := make([]transcript.Turn, 0, len(s.Turns))
		for _, t := range s.Turns {
			if t.Role == transcript.RoleUser {
				if t.Text != "" && !impact.AutomatedPrompt(t.Text) {
					turns = append(turns, t)
				}
				continue
			}
			if t.Role != transcript.RoleTool || t.ToolTarget == "" {
				continue
			}
			k := s.Agent + "|" + t.At.UTC().Format("20060102T150405.000") + "|" + t.Tool + "|" + t.ToolTarget
			if !t.At.IsZero() {
				if seen[k] {
					continue
				}
				seen[k] = true
			}
			turns = append(turns, t)
		}
		if len(turns) == 0 {
			continue
		}
		s.Turns = turns
		for _, t := range turns {
			if t.Role == transcript.RoleTool {
				n++
			}
		}
		out = append(out, s)
	}
	return Corpus{Sessions: out, Calls: n}
}

// Run backtests compiled checks over the corpus. One pass over the sessions,
// in parallel; each check sees each session's calls in order.
func Run(cs []*Compiled, corpus Corpus) []Backtest {
	res := make([]Backtest, len(cs))
	type partial struct {
		scanned, actions, matches []int
		sessions                  []int
		samples                   [][]Match
	}
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	jobs := make(chan transcript.Session)
	parts := make([]partial, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		p := &parts[w]
		p.scanned, p.actions, p.matches, p.sessions = make([]int, len(cs)), make([]int, len(cs)), make([]int, len(cs)), make([]int, len(cs))
		p.samples = make([][]Match, len(cs))
		wg.Add(1)
		go func() {
			defer wg.Done()
			for s := range jobs {
				hit := make([]bool, len(cs))
				var history []string
				prompt := ""
				for _, t := range s.Turns {
					if t.Role == transcript.RoleUser {
						prompt = clip(strings.Join(strings.Fields(t.Text), " "), 300)
						continue
					}
					for ci, c := range cs {
						if !c.AppliesTo(t.Tool, s.Cwd, s.Agent) {
							continue
						}
						p.scanned[ci]++
						if !c.IsAction(t.ToolTarget) {
							continue
						}
						p.actions[ci]++
						if c.Violates(t.ToolTarget, history) {
							p.matches[ci]++
							if !hit[ci] {
								hit[ci] = true
								p.sessions[ci]++
							}
							if len(p.samples[ci]) < MaxKeptMatches {
								p.samples[ci] = append(p.samples[ci], Match{Session: s.ID, Agent: s.Agent, Cwd: s.Cwd,
									Tool: t.Tool, Target: clip(t.ToolTarget, 400), At: t.At, Earlier: len(history), Prompt: prompt})
							}
						}
					}
					if len(history) >= MaxHistory {
						history = history[1:]
					}
					history = append(history, t.ToolTarget)
				}
			}
		}()
	}
	for _, s := range corpus.Sessions {
		jobs <- s
	}
	close(jobs)
	wg.Wait()
	for ci := range cs {
		b := &res[ci]
		for _, p := range parts {
			b.Scanned += p.scanned[ci]
			b.Actions += p.actions[ci]
			b.Matches += p.matches[ci]
			b.Sessions += p.sessions[ci]
			b.Samples = append(b.Samples, p.samples[ci]...)
		}
		if b.Scanned > 0 {
			b.Rate = float64(b.Matches) / float64(b.Scanned)
		}
		sort.Slice(b.Samples, func(i, j int) bool { return b.Samples[i].At.Before(b.Samples[j].At) })
	}
	return res
}

// Wilson returns the 95% Wilson score interval for k successes in n trials.
func Wilson(k, n int) (lo, hi float64) {
	if n == 0 {
		return 0, 1
	}
	const z = 1.959964
	p := float64(k) / float64(n)
	nn := float64(n)
	den := 1 + z*z/nn
	centre := (p + z*z/(2*nn)) / den
	half := z * math.Sqrt(p*(1-p)/nn+z*z/(4*nn*nn)) / den
	return math.Max(0, centre-half), math.Min(1, centre+half)
}
