package api

import (
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
	"github.com/JeremiahM37/grimoire/go/internal/exp"
	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// Datamarking on the recall endpoint (experiment flag "guard").
//
// Untrusted recall entries have every space in their text replaced by a
// per-entry random marker, and the entry carries that marker in its own field,
// which states the rule. See internal/trust/spotlight.go for what the marking
// does and does not promise.
//
// It runs AFTER assessHits and markProcedures, because those classify and rank
// on the text and a marked text would classify differently. It is wired into
// the recall handler only: other memory endpoints still return unmarked text.
//
// If a token cannot be drawn the entry's text is withheld rather than returned
// unmarked, so a failure here fails closed.
func markUntrustedRecall(out []entryOut) {
	if !trust.SpotlightOn() {
		return
	}
	for i := range out {
		if out[i].Trust != trust.NameUntrusted {
			continue
		}
		token, field, err := trust.NewMark()
		if err != nil {
			out[i].Text = "[untrusted text withheld: datamarking unavailable]"
			continue
		}
		out[i].Text = trust.Datamark(out[i].Text, token)
		out[i].Marker = field
	}
}

// guardThreshold is the decision-model probability above which an untrusted
// line is reported. It is a placeholder until the lab's calibration fixes it
// (docs/lab/guard.md); it is not tuned on the benchmark sets.
const guardThreshold = 0.5

// guardClassifier is the decision-model signal for the dream sweep. It is nil
// unless the "guard" experiment is on and a decision server is configured, and
// nil means the pattern scanner alone (secscan fails open).
func (s *Server) guardClassifier() *secscan.Classifier {
	if !exp.On("guard") {
		return nil
	}
	c := s.decisionClient()
	if c == nil {
		return nil
	}
	return &secscan.Classifier{Client: c, Threshold: guardThreshold}
}
