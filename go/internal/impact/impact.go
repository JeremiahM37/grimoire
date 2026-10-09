// Package impact measures friction in agent sessions before and after a memory
// or a skill landed.
//
// This is correlation, not evidence of effect. Sessions are not randomised,
// the work changes over time, agents and models change, and a memory is
// usually written in response to a problem, which makes "before" the worse
// period by construction. Every figure here carries its session counts, uses
// medians (a few huge sessions do not move them), and the report says so in
// words. A difference smaller than the spread of the sessions on either side
// is reported as no clear difference.
package impact

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Caveat is attached to every report.
const Caveat = "Correlation only: sessions before and after are different work, and a memory is usually written because " +
	"something went wrong, so the 'before' side is expected to look worse. Use this to find what to look at, not to claim an effect."

// Friction is what one session cost the person.
type Friction struct {
	Agent       string
	ID          string
	Start       time.Time
	Prompts     int
	ToolCalls   int
	ToolErrors  int
	Corrections int // user turns that push back ("no, ...", "I told you")
	Retells     int // user turns that repeat an earlier one in the same session
	Minutes     float64
	terms       map[string]bool
}

var (
	pushbackStart = regexp.MustCompile(`(?i)^\W*(no|nope|wrong|stop|wait|don'?t|do not|not that|that'?s not|that is not|incorrect|undo|revert)\b`)
	pushbackAny   = regexp.MustCompile(`(?i)\b(i (already )?(told|said|asked)( you)?|as i (said|mentioned)|like i said|you (forgot|missed|didn'?t|did not|ignored)|why did you|that'?s wrong|that is wrong|i meant|not what i (asked|wanted))\b`)
	wordRE        = regexp.MustCompile(`[a-z0-9][a-z0-9_.-]{2,}`)
)

// Stats computes the friction of each session.
func Stats(sessions []transcript.Session) []Friction {
	out := make([]Friction, 0, len(sessions))
	for _, s := range sessions {
		f := Friction{Agent: s.Agent, ID: s.ID, Start: s.Start, Prompts: s.Prompts(), ToolCalls: s.ToolCalls(),
			ToolErrors: s.ToolErrors(), Minutes: s.Duration().Minutes(), terms: map[string]bool{}}
		var earlier []map[string]bool
		first := true
		for _, t := range s.Turns {
			switch t.Role {
			case transcript.RoleUser:
				words := wordSet(t.Text)
				if !first {
					if pushbackStart.MatchString(t.Text) || pushbackAny.MatchString(t.Text) {
						f.Corrections++
					}
					for _, e := range earlier {
						if len(words) >= 4 && jaccard(words, e) >= 0.6 {
							f.Retells++
							break
						}
					}
				}
				first = false
				earlier = append(earlier, words)
				for w := range words {
					f.terms[w] = true
				}
			case transcript.RoleTool:
				for w := range wordSet(t.ToolTarget) {
					f.terms[w] = true
				}
			}
		}
		out = append(out, f)
	}
	return out
}

func wordSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range wordRE.FindAllString(strings.ToLower(s), -1) {
		out[w] = true
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return float64(n) / float64(len(a)+len(b)-n)
}

// Item is a memory or a skill with the date it landed.
type Item struct {
	Kind  string // "memory" or "skill"
	Name  string
	Agent string // for a skill: the agent that loads it (empty = all agents)
	At    time.Time
	Terms []string // what the item is about, for the topical subset
	// DateSource says where At came from: "frontmatter", "file-mtime", "exported".
	DateSource string
}

// Side summarises the sessions on one side of a date.
type Side struct {
	Sessions       int     `json:"sessions"`
	MedianErrors   float64 `json:"median_tool_errors"`
	MedianPrompts  float64 `json:"median_prompts"`
	MedianCorrect  float64 `json:"median_corrections"`
	MedianRetells  float64 `json:"median_retells"`
	MedianMinutes  float64 `json:"median_minutes"`
	ErrorsPer100   float64 `json:"tool_errors_per_100_calls"` // pooled, for scale
	SessionsWithEr float64 `json:"share_with_tool_error"`
}

// Comparison is before versus after for one set of sessions.
type Comparison struct {
	Before  Side   `json:"before"`
	After   Side   `json:"after"`
	Verdict string `json:"verdict"`
}

// Row is one item's result.
type Row struct {
	Kind       string      `json:"kind"`
	Name       string      `json:"name"`
	Agent      string      `json:"agent,omitempty"`
	LandedAt   time.Time   `json:"landed_at"`
	DateSource string      `json:"date_source"`
	All        Comparison  `json:"all_sessions"`
	Topical    *Comparison `json:"topical_sessions,omitempty"`
}

// Report is the whole answer.
type Report struct {
	Caveat   string    `json:"caveat"`
	Sessions int       `json:"sessions_considered"`
	Since    time.Time `json:"since,omitempty"`
	MinSide  int       `json:"min_sessions_per_side"`
	Agents   []string  `json:"agents"`
	Rows     []Row     `json:"items"`
}

// Options shapes Compute.
type Options struct {
	MinPerSide int // sessions needed on each side to say anything (default 5)
}

// Compute compares sessions before and after each item's date.
func Compute(items []Item, stats []Friction, opt Options) Report {
	if opt.MinPerSide <= 0 {
		opt.MinPerSide = 5
	}
	rep := Report{Caveat: Caveat, Sessions: len(stats), MinSide: opt.MinPerSide, Rows: []Row{}}
	agents := map[string]bool{}
	for _, s := range stats {
		agents[s.Agent] = true
	}
	for a := range agents {
		rep.Agents = append(rep.Agents, a)
	}
	sort.Strings(rep.Agents)
	for _, it := range items {
		if it.At.IsZero() {
			continue
		}
		pool := stats
		if it.Agent != "" {
			pool = nil
			for _, s := range stats {
				if s.Agent == it.Agent {
					pool = append(pool, s)
				}
			}
		}
		row := Row{Kind: it.Kind, Name: it.Name, Agent: it.Agent, LandedAt: it.At, DateSource: it.DateSource}
		row.All = compare(pool, it.At, opt.MinPerSide)
		if terms := topicTerms(it.Terms); len(terms) > 0 {
			var topical []Friction
			need := min(2, len(terms))
			for _, s := range pool {
				n := 0
				for _, t := range terms {
					if s.terms[t] {
						n++
					}
				}
				if n >= need {
					topical = append(topical, s)
				}
			}
			c := compare(topical, it.At, opt.MinPerSide)
			row.Topical = &c
		}
		rep.Rows = append(rep.Rows, row)
	}
	sort.SliceStable(rep.Rows, func(i, j int) bool { return rep.Rows[i].LandedAt.After(rep.Rows[j].LandedAt) })
	return rep
}

func topicTerms(in []string) []string {
	stop := map[string]bool{"the": true, "and": true, "for": true, "with": true, "how": true, "use": true, "when": true, "this": true,
		"that": true, "from": true, "into": true, "never": true, "always": true, "not": true, "are": true, "you": true, "your": true}
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		for w := range wordSet(s) {
			if len(w) >= 4 && !stop[w] && !seen[w] {
				seen[w] = true
				out = append(out, w)
			}
		}
	}
	sort.Strings(out)
	if len(out) > 12 {
		out = out[:12]
	}
	return out
}

func compare(pool []Friction, at time.Time, minSide int) Comparison {
	var before, after []Friction
	for _, s := range pool {
		if s.Start.IsZero() {
			continue
		}
		if s.Start.Before(at) {
			before = append(before, s)
		} else {
			after = append(after, s)
		}
	}
	c := Comparison{Before: summarise(before), After: summarise(after)}
	switch {
	case len(before) < minSide || len(after) < minSide:
		c.Verdict = "not enough sessions on both sides to compare"
	default:
		c.Verdict = verdict(before, after)
	}
	return c
}

func summarise(fs []Friction) Side {
	s := Side{Sessions: len(fs)}
	if len(fs) == 0 {
		return s
	}
	col := func(f func(Friction) float64) []float64 {
		v := make([]float64, len(fs))
		for i, x := range fs {
			v[i] = f(x)
		}
		return v
	}
	s.MedianErrors = median(col(func(f Friction) float64 { return float64(f.ToolErrors) }))
	s.MedianPrompts = median(col(func(f Friction) float64 { return float64(f.Prompts) }))
	s.MedianCorrect = median(col(func(f Friction) float64 { return float64(f.Corrections) }))
	s.MedianRetells = median(col(func(f Friction) float64 { return float64(f.Retells) }))
	s.MedianMinutes = median(col(func(f Friction) float64 { return f.Minutes }))
	calls, errs, withErr := 0, 0, 0
	for _, f := range fs {
		calls += f.ToolCalls
		errs += f.ToolErrors
		if f.ToolErrors > 0 {
			withErr++
		}
	}
	if calls > 0 {
		s.ErrorsPer100 = round1(100 * float64(errs) / float64(calls))
	}
	s.SessionsWithEr = round2(float64(withErr) / float64(len(fs)))
	return s
}

// verdict states a direction only when the error and correction rates agree
// and the gap is larger than half the spread of either side; otherwise it
// says there is no clear difference. It is deliberately conservative.
func verdict(before, after []Friction) string {
	rate := func(fs []Friction, f func(Friction) float64) (med, spread float64) {
		v := make([]float64, len(fs))
		for i, x := range fs {
			v[i] = f(x)
		}
		return median(v), iqr(v)
	}
	metrics := map[string]func(Friction) float64{
		"tool errors per session": func(f Friction) float64 { return float64(f.ToolErrors) },
		"corrections per session": func(f Friction) float64 { return float64(f.Corrections) },
		"prompts per session":     func(f Friction) float64 { return float64(f.Prompts) },
	}
	names := make([]string, 0, len(metrics))
	for n := range metrics {
		names = append(names, n)
	}
	sort.Strings(names)
	var lower, higher []string
	for _, n := range names {
		bm, bs := rate(before, metrics[n])
		am, as := rate(after, metrics[n])
		gap := am - bm
		if math.Abs(gap) > 0 && math.Abs(gap) >= 0.5*math.Max(bs, as) && math.Abs(gap) >= 0.5 {
			if gap < 0 {
				lower = append(lower, n)
			} else {
				higher = append(higher, n)
			}
		}
	}
	switch {
	case len(lower) > 0 && len(higher) == 0:
		return "lower after (" + strings.Join(lower, ", ") + "); correlation only"
	case len(higher) > 0 && len(lower) == 0:
		return "higher after (" + strings.Join(higher, ", ") + "); correlation only"
	case len(lower) > 0 && len(higher) > 0:
		return "mixed: lower " + strings.Join(lower, ", ") + "; higher " + strings.Join(higher, ", ")
	}
	return "no clear difference"
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	if n := len(s); n%2 == 1 {
		return s[n/2]
	} else {
		return (s[n/2-1] + s[n/2]) / 2
	}
}

func iqr(v []float64) float64 {
	if len(v) < 4 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)*3/4] - s[len(s)/4]
}

func round1(f float64) float64 { return math.Round(f*10) / 10 }
func round2(f float64) float64 { return math.Round(f*100) / 100 }
