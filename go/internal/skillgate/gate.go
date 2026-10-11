package skillgate

import (
	"fmt"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/replay"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Options are the gate's thresholds. Zero values take the defaults.
type Options struct {
	MinSupport     int     // independent sessions containing the pattern (default 3)
	MinHeldOut     int     // sessions without the pattern needed to measure false fires (default 5)
	MaxFalseFire   float64 // share of held-out sessions the trigger may fire on (default 0.2)
	MaxFailedShare float64 // share of the pattern's sessions with a failed outcome (default 0.5)
	// MinRel is the replay engine's relevance floor. The gate has no
	// embeddings, so relevance is 0.4 x the share of trigger terms present in
	// the session text: 0.4 means every term must appear (default 0.4).
	MinRel float64
}

func (o *Options) defaults() {
	if o.MinSupport <= 0 {
		o.MinSupport = 3
	}
	if o.MinHeldOut <= 0 {
		o.MinHeldOut = 5
	}
	if o.MaxFalseFire <= 0 {
		o.MaxFalseFire = 0.2
	}
	if o.MaxFailedShare <= 0 {
		o.MaxFailedShare = 0.5
	}
	if o.MinRel <= 0 {
		o.MinRel = 0.4
	}
}

// Check runs one candidate spec against recorded sessions. Sessions that
// contain the pattern are its sources; every other session is held out, and
// the trigger is replayed against each to count false fires. The result is a
// verdict with its numbers, not a decision to store anything.
func Check(spec Spec, pattern []string, sessions []transcript.Session, opt Options) skillmine.Gate {
	opt.defaults()
	g := skillmine.Gate{Trigger: spec.Trigger}

	seen := map[string]bool{}
	for _, s := range sessions {
		key := sessionKey(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		units := skillmine.Units(s)
		if skillmine.ContainsRun(units, pattern) {
			g.Sources = append(g.Sources, key)
			if known, failed := skillmine.Outcome(s); known {
				g.KnownSources++
				if failed {
					g.FailedSources++
				}
			}
			continue
		}
		g.HeldOut++
		if fires(spec.Trigger, s, opt.MinRel) {
			g.HeldOutFires++
		}
	}
	g.Support = len(g.Sources)
	if g.HeldOut > 0 {
		g.FalseFireRate = float64(g.HeldOutFires) / float64(g.HeldOut)
	}
	g.Pass, g.Reasons = verdict(spec, g, opt)
	return g
}

func verdict(spec Spec, g skillmine.Gate, opt Options) (bool, []string) {
	var why []string
	if strings.TrimSpace(spec.Trigger) == "" {
		why = append(why, "no trigger to test")
	}
	if g.Support < opt.MinSupport {
		why = append(why, fmt.Sprintf("support: %d independent session(s) contain the pattern, need %d", g.Support, opt.MinSupport))
	}
	switch {
	case g.HeldOut < opt.MinHeldOut:
		why = append(why, fmt.Sprintf("false fires not measured: %d held-out session(s), need %d", g.HeldOut, opt.MinHeldOut))
	case g.FalseFireRate > opt.MaxFalseFire:
		why = append(why, fmt.Sprintf("trigger over-broad: fired on %d of %d held-out sessions without the pattern (%.0f%%, limit %.0f%%)",
			g.HeldOutFires, g.HeldOut, 100*g.FalseFireRate, 100*opt.MaxFalseFire))
	}
	if g.KnownSources > 0 && float64(g.FailedSources)/float64(g.KnownSources) > opt.MaxFailedShare {
		why = append(why, fmt.Sprintf("failure contamination: %d of %d sessions with a recorded outcome ended in failure, learned mostly from failures",
			g.FailedSources, g.KnownSources))
	}
	return len(why) == 0, why
}

// fires reports whether the trigger fires on one session. The replay engine
// scores a situation's terms against a memory's, so the trigger is the
// situation and the session is the memory: the trigger fires when its terms
// reach the floor in the session's text.
func fires(trigger string, s transcript.Session, minRel float64) bool {
	if strings.TrimSpace(trigger) == "" {
		return false
	}
	const target = "note:session"
	text := sessionText(s)
	sn := replay.NewSnapshot([]*replay.Item{replay.NewItem(target, text, []string{text}, nil)}, nil)
	p := replay.Prepare(replay.Situation{ID: trigger, Text: trigger, Stage: "prompt", MinRel: minRel})
	for _, sc := range sn.Score(p, map[string]bool{target: true}) {
		if sc.Score >= p.MinRel {
			return true
		}
	}
	return false
}

// sessionText is what a session is about, for matching a trigger: what the
// person asked and the commands and paths the agent acted on.
func sessionText(s transcript.Session) string {
	var b strings.Builder
	for _, t := range s.Turns {
		switch t.Role {
		case transcript.RoleUser:
			b.WriteString(t.Text)
		case transcript.RoleTool:
			b.WriteString(" " + t.ToolTarget)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// sessionKey names a session in provenance, so it must read as text in the
// frontmatter: no separators that YAML or the list format would mangle.
func sessionKey(s transcript.Session) string {
	if s.ID == "" {
		return s.Agent + "/" + s.Path
	}
	return s.Agent + "/" + s.ID
}

// Apply gates every candidate against the sessions it was mined from and the
// rest of them, recording each verdict on the candidate.
func Apply(cands []skillmine.Candidate, sessions []transcript.Session, opt Options) {
	for i := range cands {
		spec := FromCandidate(cands[i])
		g := Check(spec, cands[i].Units, sessions, opt)
		cands[i].Gate = &g
	}
}
