package rulecheck

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
)

// SamplePerRule is the most matches labelled for one rule, split across its
// checks.
const SamplePerRule = 20

// Question is what the labelling model is asked about one flagged call.
var Question = map[string]decide.Question{"violation": {
	Type: decide.Noul,
	Instructions: "A standing rule for a coding agent and one tool call the agent made are given, with the check that flagged the call. " +
		"Did this call actually break the rule? Answer yes only if the call does what the rule forbids, or skips what the rule requires " +
		"(for a before-requirement, the earlier-call list given is complete). Answer no if the call is harmless, unrelated, only mentions " +
		"the forbidden thing as text, or falls outside what the rule is about.",
	Criteria: map[string]string{
		"true":  "the call breaks the rule",
		"false": "the call is allowed by the rule, or the check flagged it by accident",
	},
}}

// FaithfulQuestion asks whether a check follows from its rule at all. A model
// asked only "did this call break the rule" tends to say yes to whatever the
// check flagged, so a check that is a poor reading of the rule (a required flag
// the rule never mentions) is rejected here before any of its matches are
// labelled.
var FaithfulQuestion = map[string]decide.Question{"faithful": {
	Type: decide.Noul,
	Instructions: "A standing rule for a coding agent and a mechanical check over its tool calls are given. " +
		"Is the check a faithful reading of the rule: does every call the check flags follow from what the rule actually says, " +
		"with nothing invented (no required flag, command or file the rule does not mention)? Answer no if the check requires or forbids something " +
		"the rule text does not state, or only shares a topic with the rule.",
	Criteria: map[string]string{
		"true":  "the check says what the rule says",
		"false": "the check invents a requirement, or is only loosely related to the rule",
	},
}}

// FaithfulState renders the input for FaithfulQuestion.
func FaithfulState(ruleText string, sp Spec) string {
	return "Rule:\n" + clip(ruleText, 900) + "\n\nCheck: " + sp.Describe() + " over tools " + strings.Join(append([]string{}, orBash(sp.Tools)...), ",")
}

func orBash(t []string) []string {
	if len(t) == 0 {
		return DefaultTools
	}
	return t
}

// Faithful returns P(the check is a faithful reading of the rule).
func (l *Labeller) Faithful(ruleText string, sp Spec) (float64, bool) {
	return l.ask("faithful", FaithfulQuestion, FaithfulState(ruleText, sp))
}

// StateFor renders the labelling input for one match.
func StateFor(ruleText string, sp Spec, m Match) string {
	var b strings.Builder
	b.WriteString("Rule:\n" + clip(ruleText, 900) + "\n\nCheck: " + sp.Describe())
	switch sp.Shape {
	case ShapeForbid:
		b.WriteString("\n(the check flags any call matching the pattern)")
	case ShapeRequireBefore:
		b.WriteString(fmt.Sprintf("\n(the check flags a call matching the action when none of the %d earlier calls in the session matched the required step)", m.Earlier))
	case ShapeRequireWith:
		b.WriteString("\n(the check flags a call matching the action that lacks the required part)")
	}
	b.WriteString("\n\nTool call (" + m.Tool + ")")
	if m.Cwd != "" {
		b.WriteString(" in " + m.Cwd)
	}
	b.WriteString(":\n" + clip(m.Target, 400))
	if m.Prompt != "" {
		b.WriteString("\n\nThe user's latest message before this call (a rule that says \"unless asked\" is not broken by a call the user asked for):\n" + clip(m.Prompt, 300))
	}
	return b.String()
}

// Labeller asks a decision model whether flagged calls were true violations,
// with an on-disk cache so a re-run costs nothing for what was seen.
type Labeller struct {
	Client    *decide.Client
	Threshold float64 // P(yes) at or above which a match is a true violation
	Budget    int     // most NEW calls
	CachePath string
	Workers   int

	mu    sync.Mutex
	cache map[string]float64
	Calls int
	Hits  int
	Errs  int
}

func labelKey(model, state string) string {
	h := sha256.Sum256([]byte(model + "\x00" + state))
	return hex.EncodeToString(h[:12])
}

func (l *Labeller) load() {
	if l.cache != nil {
		return
	}
	l.cache = map[string]float64{}
	if l.CachePath == "" {
		return
	}
	f, err := os.Open(l.CachePath)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		var r struct {
			K string  `json:"k"`
			P float64 `json:"p"`
		}
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.K != "" {
			l.cache[r.K] = r.P
		}
	}
}

func (l *Labeller) save(k string, p float64) {
	if l.CachePath == "" {
		return
	}
	f, err := os.OpenFile(l.CachePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]any{"k": k, "p": p})
	f.Write(append(b, '\n'))
}

// Probability returns P(true violation) for one state, from cache or the
// model. ok is false when the budget is spent or the model failed.
func (l *Labeller) Probability(state string) (float64, bool) {
	return l.ask("violation", Question, state)
}

func (l *Labeller) ask(name string, q map[string]decide.Question, state string) (float64, bool) {
	l.mu.Lock()
	l.load()
	k := labelKey(l.Client.Model+"|"+name, state)
	if p, ok := l.cache[k]; ok {
		l.Hits++
		l.mu.Unlock()
		return p, true
	}
	if l.Calls >= l.Budget {
		l.mu.Unlock()
		return 0, false
	}
	l.Calls++
	l.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := l.Client.Ask(ctx, state, q)
	a, ok := res.Answers[name]
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil || !ok || a.Type != decide.Noul {
		l.Errs++
		return 0, false
	}
	l.cache[k] = a.Noul
	l.save(k, a.Noul)
	return a.Noul, true
}

// Pick chooses up to n matches spread over distinct sessions first, then by
// time, deterministically.
func Pick(ms []Match, n int) []Match {
	if len(ms) <= n {
		return ms
	}
	bySession := map[string][]Match{}
	var order []string
	for _, m := range ms {
		if _, ok := bySession[m.Session]; !ok {
			order = append(order, m.Session)
		}
		bySession[m.Session] = append(bySession[m.Session], m)
	}
	sort.Strings(order)
	var out []Match
	for round := 0; len(out) < n; round++ {
		added := false
		for _, s := range order {
			if round < len(bySession[s]) && len(out) < n {
				out = append(out, bySession[s][round])
				added = true
			}
		}
		if !added {
			break
		}
	}
	return out
}

// Labelled is the result of labelling a sample.
type Labelled struct {
	N, True int
	Probs   []float64
	Items   []LabelledMatch
}

// LabelledMatch pairs a match with the model's probability.
type LabelledMatch struct {
	Match
	P    float64 `json:"p"`
	True bool    `json:"true_violation"`
}

// Label labels a sample of matches for one check.
func (l *Labeller) Label(ruleText string, sp Spec, sample []Match) Labelled {
	out := Labelled{}
	type res struct {
		p  float64
		ok bool
	}
	rs := make([]res, len(sample))
	w := l.Workers
	if w <= 0 {
		w = 6
	}
	sem := make(chan struct{}, w)
	var wg sync.WaitGroup
	for i, m := range sample {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, m Match) {
			defer wg.Done()
			defer func() { <-sem }()
			p, ok := l.Probability(StateFor(ruleText, sp, m))
			rs[i] = res{p, ok}
		}(i, m)
	}
	wg.Wait()
	for i, r := range rs {
		if !r.ok {
			continue
		}
		out.N++
		t := r.p >= l.Threshold
		if t {
			out.True++
		}
		out.Probs = append(out.Probs, r.p)
		out.Items = append(out.Items, LabelledMatch{Match: sample[i], P: r.p, True: t})
	}
	return out
}
