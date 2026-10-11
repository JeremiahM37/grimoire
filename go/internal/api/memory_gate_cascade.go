package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cascade"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// cascadeTimeout bounds one gate decision on the write path. Laya runs on the
// box and answers in well under this; Jev is a network call.
const cascadeTimeout = 5 * time.Second

// cascadeGate builds the gate from settings. A stage with no URL is off, so a
// gate with neither stage always keeps both facts.
func (s *Server) cascadeGate() cascade.Gate {
	var g cascade.Gate
	g.LayaCal, _ = cascade.ParsePlatt(s.setting("cascade_laya_calibration"))
	g.JevCal, _ = cascade.ParsePlatt(s.setting("cascade_jev_calibration"))
	if g.LayaCal == (cascade.Platt{}) {
		g.LayaCal = cascade.Identity
	}
	if g.JevCal == (cascade.Platt{}) {
		g.JevCal = cascade.Identity
	}
	g.Lo, g.Hi = 0.05, 0.65
	if lo, hi, err := cascade.ParseBand(s.setting("cascade_band")); err == nil {
		g.Lo, g.Hi = lo, hi
	}
	if url := strings.TrimSpace(s.setting("cascade_laya_url")); url != "" {
		g.Laya = cascade.ClientAsker{C: &decide.Client{BaseURL: url,
			Model: s.setting("cascade_laya_model"), HTTP: &http.Client{Timeout: cascadeTimeout}}}
	}
	if url := strings.TrimSpace(s.setting("decision_url")); url != "" {
		key := strings.TrimSpace(s.setting("decision_api_key"))
		if key == "" && s.Secrets != nil {
			if v, err := s.Secrets.Get("decision-api-key"); err == nil {
				key = v
			}
		}
		g.Jev = cascade.ClientAsker{C: &decide.Client{BaseURL: url,
			Model: s.setting("decision_model"), APIKey: key, HTTP: &http.Client{Timeout: cascadeTimeout}}}
	}
	return g
}

// cascadeDecide answers a contested supersession. It is only reached behind
// the `cascade` flag, after the rules have deferred the pair.
func (s *Server) cascadeDecide(old, new string) cascade.Verdict {
	ctx, cancel := context.WithTimeout(context.Background(), cascadeTimeout*2)
	defer cancel()
	return s.cascadeGate().Decide(ctx, old, new)
}

// cascadeReconcile is the `cascade` experiment's replacement gate on the write
// path. A supersession of a contested pair is a replacement the gate must
// confirm, whichever decided it: the rules (which defer it) or the model. Off
// the flag, the decision passes through untouched.
func (s *Server) cascadeReconcile(fact string, d memory.Decision, byID map[string]index.MemoryHit) memory.Decision {
	if !exp.On("cascade") {
		return d
	}
	id := d.Contested
	if id == "" && d.Op == memory.OpUpdate && d.Target != "" && memory.Contested(fact, byID[d.Target].Text) {
		id = d.Target
	}
	target, ok := byID[id]
	if id == "" || !ok {
		return d
	}
	if v := s.cascadeDecide(target.Text, fact); v.Replace {
		return memory.Decision{Op: memory.OpUpdate, Text: fact, Target: target.ID,
			Why: "cascade (" + v.Source + "): supersedes: " + target.Text}
	}
	return memory.Decision{Op: memory.OpAdd, Text: fact, Contested: target.ID,
		Why: "cascade (kept both): " + target.Text}
}
