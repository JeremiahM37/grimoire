// Package skillmine finds command sequences that recur across an agent's
// sessions and drafts procedure memories from them, for a person to review.
//
// The mining is deterministic: shell tool calls are normalised to a short unit
// (`git push`, `go test`, `systemctl restart`), each session becomes a list of
// units, and contiguous runs of units (n-grams) that appear in at least N
// distinct sessions are candidates. Longer runs absorb the shorter runs they
// contain when they recur as often. No model is involved. A model, when one is
// configured, may only title and describe a candidate (Titler); it never
// decides what is a candidate, and a failure of it changes nothing.
//
// Nothing here adds a memory. Candidates are drafts: Draft renders one as the
// text of a procedure note, and the caller writes them to a review queue.
package skillmine

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Options shapes Mine.
type Options struct {
	MinSessions int // distinct sessions a run must appear in (default 3)
	MinLen      int // shortest run, in units (default 3)
	MaxLen      int // longest run (default 8)
	Limit       int // candidates returned (default 20)
}

func (o *Options) defaults() {
	if o.MinSessions <= 0 {
		o.MinSessions = 3
	}
	if o.MinLen <= 0 {
		o.MinLen = 3
	}
	if o.MaxLen < o.MinLen {
		o.MaxLen = max(8, o.MinLen)
	}
	if o.Limit <= 0 {
		o.Limit = 20
	}
}

// Candidate is a recurring command sequence.
type Candidate struct {
	ID          string
	Units       []string // normalised steps, e.g. "git status"
	Examples    []string // the most common concrete command for each step
	Sessions    int
	Agents      map[string]int
	Projects    []string // working directories' last element, most common first
	First, Last time.Time
	Score       float64

	Title       string // deterministic unless a Titler replaced it
	Description string
	Titled      bool // a model wrote Title/Description
}

// Titler may rewrite a candidate's title and description. It is given the
// deterministic ones and must not be trusted with anything else.
type Titler func(c Candidate) (title, description string, err error)

// shellTool reports whether a tool name runs a shell command.
func shellTool(name string) bool {
	n := strings.ToLower(name)
	return n == "bash" || n == "shell" || n == "sh" || n == "local_shell" || n == "exec_command" ||
		strings.Contains(n, "terminal_command") || n == "run_shell_command" || n == "execute_command"
}

type gramStats struct {
	units    []string
	sessions map[int]bool
	examples []map[string]int
	agents   map[string]int
	projects map[string]int
	first    time.Time
	last     time.Time
}

// Mine finds the recurring command runs in sessions.
func Mine(sessions []transcript.Session, opt Options) []Candidate {
	opt.defaults()
	grams := map[string]*gramStats{}
	for si, s := range sessions {
		units, cmds := sequence(s)
		if len(units) < opt.MinLen {
			continue
		}
		proj := filepath.Base(s.Cwd)
		seen := map[string]bool{}
		for n := opt.MinLen; n <= opt.MaxLen && n <= len(units); n++ {
			for i := 0; i+n <= len(units); i++ {
				run := units[i : i+n]
				if !worthy(run) {
					continue
				}
				key := strings.Join(run, "\x1f")
				g := grams[key]
				if g == nil {
					g = &gramStats{units: append([]string(nil), run...), sessions: map[int]bool{},
						agents: map[string]int{}, projects: map[string]int{}}
					for range run {
						g.examples = append(g.examples, map[string]int{})
					}
					grams[key] = g
				}
				if seen[key] {
					continue // one vote per session
				}
				seen[key] = true
				g.sessions[si] = true
				g.agents[s.Agent]++
				if proj != "" && proj != "." && proj != "/" {
					g.projects[proj]++
				}
				for j := range run {
					g.examples[j][cmds[i+j]]++
				}
				if !s.Start.IsZero() && (g.first.IsZero() || s.Start.Before(g.first)) {
					g.first = s.Start
				}
				if s.End.After(g.last) {
					g.last = s.End
				}
			}
		}
	}
	var keep []*gramStats
	for _, g := range grams {
		if len(g.sessions) >= opt.MinSessions {
			keep = append(keep, g)
		}
	}
	keep = absorb(keep)
	var out []Candidate
	for _, g := range keep {
		c := Candidate{Units: g.units, Sessions: len(g.sessions), Agents: g.agents, First: g.first, Last: g.last}
		for _, ex := range g.examples {
			c.Examples = append(c.Examples, top(ex, 1)[0])
		}
		c.Projects = top(g.projects, 3)
		c.ID = idOf(g.units)
		c.Score = float64(c.Sessions) * float64(len(c.Units))
		c.Title, c.Description = defaultTitle(c)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	if len(out) > opt.Limit {
		out = out[:opt.Limit]
	}
	return out
}

// Retitle applies a Titler to each candidate; any error leaves that one as it was.
func Retitle(cs []Candidate, t Titler) {
	if t == nil {
		return
	}
	for i := range cs {
		title, desc, err := t(cs[i])
		title, desc = strings.TrimSpace(title), strings.TrimSpace(desc)
		if err != nil || title == "" {
			continue
		}
		if len([]rune(title)) > 80 {
			title = string([]rune(title)[:80])
		}
		cs[i].Title = title
		if desc != "" {
			cs[i].Description = desc
		}
		cs[i].Titled = true
	}
}

func idOf(units []string) string {
	h := sha256.Sum256([]byte(strings.Join(units, "\x1f")))
	return hex.EncodeToString(h[:])[:8]
}

// sequence reduces a session to its successful shell commands, normalised,
// with consecutive repeats collapsed.
func sequence(s transcript.Session) (units, cmds []string) {
	for _, t := range s.Turns {
		if t.Role != transcript.RoleTool || t.ToolError || !shellTool(t.Tool) {
			continue
		}
		for _, step := range Normalize(t.ToolTarget) {
			if len(units) > 0 && units[len(units)-1] == step.Unit {
				continue
			}
			units = append(units, step.Unit)
			cmds = append(cmds, step.Command)
		}
	}
	return units, cmds
}

// worthy: a run of one repeated unit, or of units that are all one program, is
// not a procedure.
func worthy(run []string) bool {
	distinct := map[string]bool{}
	for _, u := range run {
		distinct[u] = true
	}
	return len(distinct) >= 2
}

// absorb drops a run when a longer run containing it recurs in as many sessions.
func absorb(gs []*gramStats) []*gramStats {
	sort.Slice(gs, func(i, j int) bool { return len(gs[i].units) > len(gs[j].units) })
	var out []*gramStats
	for _, g := range gs {
		covered := false
		for _, big := range out {
			if len(big.units) > len(g.units) && len(big.sessions) >= len(g.sessions) && contains(big.units, g.units) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, g)
		}
	}
	return out
}

func contains(big, small []string) bool {
	for i := 0; i+len(small) <= len(big); i++ {
		ok := true
		for j := range small {
			if big[i+j] != small[j] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func top(m map[string]int, n int) []string {
	type kv struct {
		k string
		v int
	}
	var l []kv
	for k, v := range m {
		l = append(l, kv{k, v})
	}
	sort.Slice(l, func(i, j int) bool {
		if l[i].v != l[j].v {
			return l[i].v > l[j].v
		}
		return l[i].k < l[j].k
	})
	var out []string
	for i := 0; i < len(l) && i < n; i++ {
		out = append(out, l[i].k)
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

func defaultTitle(c Candidate) (string, string) {
	title := strings.Join(c.Units, " → ")
	if r := []rune(title); len(r) > 80 {
		title = string(r[:79]) + "…"
	}
	desc := fmt.Sprintf("A sequence of %d commands (%s) that recurred in %d sessions", len(c.Units), strings.Join(c.Units, ", "), c.Sessions)
	if len(c.Projects) > 0 && c.Projects[0] != "" {
		desc += ", mostly in " + strings.Join(c.Projects, ", ")
	}
	return title, desc + "."
}
