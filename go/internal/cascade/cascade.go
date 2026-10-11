// Package cascade decides whether a contested supersession happens, for the
// `cascade` experiment (see memory/contest.go for what "contested" means).
//
// Three stages, cheapest first:
//
//	rules   not contested: the rules decide, as they always did (not here)
//	laya    a local distilled head answers the gate question; if its
//	        calibrated probability is outside the uncertain band, it decides
//	jev     the decision model answers the same question for the pairs inside
//	        the band
//
// When neither server is configured, or a call fails or times out, the answer
// is "keep both". That is the deterministic fallback, and it is never a silent
// supersede: a wrongly struck fact is hidden from recall, a kept one is only
// a duplicate that a person can see.
package cascade

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
)

// Question is the single gate question, asked only for contested pairs.
func Question() map[string]decide.Question {
	return map[string]decide.Question{"replace": {
		Type: decide.Choice,
		Instructions: "Two statements are given as A and B. Does statement B replace statement A " +
			"(the same thing, now with a newer value, so A is no longer true), or are they about " +
			"different things, variants, parts or members of a list, that can both be true?",
		Criteria: map[string]string{
			"replace":  "B is the same thing as A with a newer value, so A is no longer true",
			"distinct": "A and B are about different things or variants, or both can be true at once",
		},
	}}
}

// State renders a pair as the state the question is asked about.
func State(old, new string) string {
	return "A: " + old + "\nB: " + new
}

// Platt is a sigmoid over the logit of a raw probability.
type Platt struct{ A, B float64 }

// Identity leaves a probability unchanged in the logit sense (a=1, b=0).
var Identity = Platt{A: 1, B: 0}

// Apply returns the calibrated probability for a raw one.
func (c Platt) Apply(p float64) float64 {
	p = math.Min(math.Max(p, 1e-6), 1-1e-6)
	return 1 / (1 + math.Exp(-(c.A*math.Log(p/(1-p)) + c.B)))
}

// ParsePlatt reads "a,b".
func ParsePlatt(v string) (Platt, error) {
	parts := strings.Split(v, ",")
	if len(parts) != 2 {
		return Platt{}, fmt.Errorf("calibration %q is not a,b", v)
	}
	a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil {
		return Platt{}, fmt.Errorf("calibration %q is not numeric", v)
	}
	return Platt{A: a, B: b}, nil
}

// ParseBand reads "lo,hi", the uncertain band in calibrated probability.
func ParseBand(v string) (lo, hi float64, err error) {
	parts := strings.Split(v, ",")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("band %q is not lo,hi", v)
	}
	lo, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	hi, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil || lo < 0 || hi > 1 || lo > hi {
		return 0, 0, fmt.Errorf("band %q is not 0<=lo<=hi<=1", v)
	}
	return lo, hi, nil
}

// Asker returns the raw probability that a pair is a replacement.
type Asker interface {
	Replace(ctx context.Context, old, new string) (float64, error)
}

// ClientAsker asks a decision server (Jev, or a Laya head behind laya-serve).
type ClientAsker struct{ C *decide.Client }

// Replace implements Asker.
func (a ClientAsker) Replace(ctx context.Context, old, new string) (float64, error) {
	res, err := a.C.Ask(ctx, State(old, new), Question())
	if err != nil {
		return 0, err
	}
	ans, ok := res.Answers["replace"]
	if !ok || ans.Type != decide.Choice {
		return 0, fmt.Errorf("decision server gave no choice answer")
	}
	if p, ok := ans.Probabilities["replace"]; ok {
		return p, nil
	}
	if ans.Choice == "replace" {
		return 1, nil
	}
	return 0, nil
}

// Gate is the configured cascade. A nil stage is off.
type Gate struct {
	Laya    Asker
	LayaCal Platt
	Jev     Asker
	JevCal  Platt
	// Lo and Hi bound the uncertain band on Laya's calibrated probability.
	// Above Hi is replace, below Lo is keep, inside is escalated to Jev.
	Lo, Hi float64
}

// Verdict is one gate decision.
type Verdict struct {
	Replace bool
	Source  string // "laya", "jev" or "fallback"
	P       float64
}

// Decide returns whether old should be superseded by new.
func (g Gate) Decide(ctx context.Context, old, new string) Verdict {
	if g.Laya != nil {
		raw, err := g.Laya.Replace(ctx, old, new)
		if err == nil {
			p := g.LayaCal.Apply(raw)
			switch {
			case p >= g.Hi:
				return Verdict{Replace: true, Source: "laya", P: p}
			case p < g.Lo:
				return Verdict{Replace: false, Source: "laya", P: p}
			}
		}
	}
	if g.Jev != nil {
		raw, err := g.Jev.Replace(ctx, old, new)
		if err == nil {
			p := g.JevCal.Apply(raw)
			return Verdict{Replace: p >= 0.5, Source: "jev", P: p}
		}
	}
	return Verdict{Replace: false, Source: "fallback"}
}
