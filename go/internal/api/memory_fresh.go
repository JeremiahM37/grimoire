package api

import (
	"context"
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/usage"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Freshness on the API surface: writes may declare a tier and a check, recall
// says per fact whether to re-check it before use, and an agent reports a
// re-check by writing the fact back (see confirmFact). The model itself is in
// memory/fresh.go.

// priorsTTL is how long the store's learned change rates are reused. They move
// only when facts are superseded, and one aggregate query every few minutes is
// noise next to a recall.
const priorsTTL = 10 * time.Minute

type freshCache struct {
	mu     sync.Mutex
	at     time.Time
	priors memory.Priors
}

// freshPriors returns the store's per-class change rates, learned from the
// facts it has seen change, cached for priorsTTL. A failed query falls back to
// the built-in rates rather than failing a recall.
func (s *Server) freshPriors() memory.Priors {
	s.fresh.mu.Lock()
	defer s.fresh.mu.Unlock()
	now := time.Now()
	if !s.fresh.at.IsZero() && now.Sub(s.fresh.at) < priorsTTL {
		return s.fresh.priors
	}
	s.fresh.priors = memory.DefaultPriors()
	if changes, exposure, err := s.Index.FreshnessEvidence(); err == nil {
		s.fresh.priors = memory.LearnPriors(changes, exposure)
	}
	s.fresh.at = now
	return s.fresh.priors
}

// forgetPriors drops the cache after a write that changes the evidence.
func (s *Server) forgetPriors() {
	s.fresh.mu.Lock()
	s.fresh.at = time.Time{}
	s.fresh.mu.Unlock()
}

// verifyThreshold is the P(stale) above which an untiered fact is marked for
// re-checking.
func (s *Server) verifyThreshold() float64 {
	if v, err := strconv.ParseFloat(s.setting("memory_verify_threshold"), 64); err == nil && v > 0 && v < 1 {
		return v
	}
	return memory.DefaultVerifyThreshold
}

// assessHits attaches a freshness assessment to each output row.
func (s *Server) assessHits(hits []index.MemoryHit, out []entryOut) []entryOut {
	if len(hits) == 0 {
		return out
	}
	p, theta, now := s.freshPriors(), s.verifyThreshold(), vault.Now()
	for i := range out {
		if i >= len(hits) || hits[i].Superseded() {
			continue
		}
		a := hits[i].Assess(now, p, theta)
		out[i].Freshness = &a
	}
	return out
}

// validateFresh checks a write's tier and check. A check is a command another
// agent will run, so it is held to the same rule the dream sweep applies to
// stored memory: nothing that would be dangerous to replay. One supplied with
// an untrusted origin is refused outright — a document other people can write
// must not be able to leave a command for an agent to run.
func validateFresh(m memoryIn) error {
	if _, _, ok := memory.ParseFresh(m.Fresh); !ok {
		return errors.New("fresh must be stable, volatile, or a re-check interval like 7d or 12h")
	}
	check := strings.TrimSpace(m.Check)
	if !memory.ValidCheck(check) {
		return errors.New("check must be one line of at most " +
			strconv.Itoa(memory.MaxCheckLen) + " characters")
	}
	if check == "" {
		return nil
	}
	if reason, bad := secscan.DangerousCommand(check); bad {
		return errors.New("check " + reason + "; a check must be a read-only lookup")
	}
	if strings.TrimSpace(m.Origin) != "" && (memory.Entry{Origin: strings.TrimSpace(m.Origin)}).Untrusted() {
		return errors.New("a check cannot be recorded from an untrusted origin")
	}
	return nil
}

// isConfirmation reports whether an explicit correction is really a re-check
// that found the value unchanged: same target, same text. Only a trusted
// writer can confirm: a pulled document "agreeing" with a fact is not a
// re-check, and would let text other people write keep a stale fact looking
// freshly verified.
func isConfirmation(m memoryIn, fact string) bool {
	if o := strings.TrimSpace(m.Origin); o != "" && (memory.Entry{Origin: o}).Untrusted() {
		return false
	}
	return m.TargetID != "" && strings.TrimSpace(m.ExpectedText) != "" &&
		memory.Normalize(fact) == memory.Normalize(m.ExpectedText)
}

// confirmFact records that an agent re-checked a fact and found it still
// true. It is a mutation of the existing bullet, not a new fact: the text and
// id stay, the confirmation count goes up, and the verified stamp moves.
func (s *Server) confirmFact(target index.MemoryHit, m memoryIn) error {
	now := vault.Now().Format(memory.StampFormat)
	err := s.mutateEntry(target.Note, target.ID, func(e *memory.Entry) {
		e.Verifies++
		e.Verified = now
		// A re-check may also be the moment the agent learns how to check.
		if c := strings.TrimSpace(m.Check); c != "" && e.Check == "" && !e.Untrusted() {
			e.Check = c
		}
		if f := strings.TrimSpace(m.Fresh); f != "" {
			e.Fresh = strings.ToLower(f)
		}
	})
	if err == nil {
		s.forgetPriors()
	}
	return err
}

// inheritFreshness carries a replaced fact's history onto its replacement.
// A supersession is one observed change unless it came within a day of the
// version it replaces (memory.CountsAsChange); that observation is the whole
// signal the learned rate runs on, so dropping it on rewrite would reset every
// fact to its class prior forever.
func inheritFreshness(e *memory.Entry, old memory.Entry) {
	if e.Vol == 0 {
		e.Vol, e.PriorRate = old.Vol, old.PriorRate
	}
	e.Changes = old.Changes
	if memory.CountsAsChange(old.Stamp, e.Stamp) {
		e.Changes++
	}
	e.Verifies = old.Verifies
	e.Since = old.ChainStart()
	if e.Fresh == "" {
		e.Fresh = old.Fresh
	}
	if e.Check == "" && !old.Untrusted() {
		e.Check = old.Check
	}
}

// decisionClient builds the typed-decision client from settings, or nil when
// none is configured. The key comes from settings or, failing that, the
// credential vault's 'decision-api-key', the same way the LLM key does.
func (s *Server) decisionClient() *decide.Client {
	url := strings.TrimSpace(s.setting("decision_url"))
	if url == "" {
		return nil
	}
	key := strings.TrimSpace(s.setting("decision_api_key"))
	if key == "" && s.Secrets != nil {
		if v, err := s.Secrets.Get("decision-api-key"); err == nil {
			key = v
		}
	}
	return &decide.Client{BaseURL: url, Model: strings.TrimSpace(s.setting("decision_model")),
		APIKey: key, HTTP: &http.Client{Timeout: decide.DefaultTimeout}}
}

// volatilityQuestion is what a decision model is asked about an untiered
// fact. Kept as one constant so an evaluation asks exactly what production
// asks.
var volatilityQuestion = map[string]decide.Question{"changes": {
	Type: decide.Noul,
	Instructions: "Does this fact describe current state that can change and go out of date " +
		"(what is running, configured, located, pending, counted, or the status of work), " +
		"rather than a fixed decision, rule, historical event, or measured finding?",
	Criteria: map[string]string{
		"true":  "current state that can change or go stale",
		"false": "a decision, rule, historical event or measured finding that stays true",
	},
}, "speed": {
	Type: decide.Choice,
	Instructions: "If what this fact describes changes, how quickly does it typically change? " +
		"Facts that never change are 'years'.",
	Criteria: map[string]string{
		"hours":  "changes within hours (live status, what is running right now)",
		"days":   "changes within a few days (task status, what is staged, test results)",
		"weeks":  "changes within weeks (work in progress, branches, plans)",
		"months": "changes within months (locations, configuration, versions, sizes)",
		"years":  "changes over years or never (decisions, rules, history, findings, hardware)",
	},
}}

// askVolatility asks the decision model whether the fact describes changing
// state and how fast, and returns the calibrated probability and the fact's
// starting change rate (memory.PriorFromDecision). Both are 0 when there is
// no model or it did not answer in time — the "nobody asked" value, so a
// failure falls back to the text's shape.
func (s *Server) askVolatility(fact string) (vol, priorRate float64) {
	c := s.decisionClient()
	if c == nil {
		return 0, 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), decide.DefaultTimeout)
	defer cancel()
	started := time.Now()
	res, err := c.Ask(ctx, fact, volatilityQuestion)
	s.bookDecision(c, res, started, err)
	if err != nil {
		return 0, 0
	}
	a, ok := res.Answers["changes"]
	if !ok || a.Type != decide.Noul {
		return 0, 0
	}
	vol = math.Max(0.01, math.Round(s.calibrate(a.Noul)*100)/100)
	// A server that does not answer the speed question (or reports only a
	// label) still gives a usable verdict; the speed just stays unknown.
	speed := res.Answers["speed"].Probabilities
	return vol, memory.PriorFromDecision(vol, speed)
}

// calibrate maps a decision model's raw probability through the configured
// Platt curve. A malformed setting leaves the probability as it came.
func (s *Server) calibrate(p float64) float64 {
	parts := strings.Split(s.setting("decision_calibration"), ",")
	if len(parts) != 2 {
		return p
	}
	a, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	b, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err1 != nil || err2 != nil {
		return p
	}
	p = math.Min(math.Max(p, 1e-4), 1-1e-4)
	return 1 / (1 + math.Exp(-(a*math.Log(p/(1-p)) + b)))
}

// bookDecision records a decision call in the usage ledger beside every other
// model call, so a hosted decision model's spend is visible where the rest of
// the AI bill is. A server on a private address is booked as local Laya.
func (s *Server) bookDecision(c *decide.Client, res decide.Result, started time.Time, err error) {
	if s.AI == nil || s.AI.Usage == nil {
		return
	}
	provider := usage.ProviderFor("openai", c.BaseURL)
	if provider != usage.TypeSafe {
		provider = usage.Laya
	}
	model := res.Model
	if model == "" {
		model = c.Model
	}
	call := usage.Call{Provider: provider, Model: model, Surface: "decide",
		InputTokens: res.InputTokens, OutputTokens: res.OutputTokens,
		LatencyMS: time.Since(started).Milliseconds()}
	if err != nil {
		call.Error = err.Error()
	}
	s.AI.Usage.Observe(call)
}
