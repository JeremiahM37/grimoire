package rulecheck

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
	"github.com/JeremiahM37/grimoire/go/internal/impact"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Rule is one standing instruction found in the memory store.
type Rule struct {
	Target string `json:"target"`
	Name   string `json:"name"`
	Text   string `json:"-"`
}

// RulesFromStore returns the notes of kind rule in a memory directory. Targets
// are note:<prefix><file>; prefix is where the store sits in the vault
// ("Agent Memory/" for the shared store).
func RulesFromStore(dir, prefix string) ([]Rule, error) {
	notes, err := memstore.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []Rule
	for _, n := range notes {
		if n.Kind != memstore.KindRule {
			continue
		}
		text := strings.TrimSpace(n.Title + "\n" + n.Description + "\n" + n.Body)
		out = append(out, Rule{Target: "note:" + prefix + n.File, Name: strings.TrimSuffix(n.File, ".md"), Text: clip(text, 3000)})
	}
	return out, nil
}

// CompileResult is what compiling a rule gave.
type CompileResult struct {
	Rule       Rule
	Candidates []Candidate
	LLMErr     error
}

// Compile classifies each rule into candidate checks: deterministic
// extraction first, then (when llm is non-nil) the local model. A rule with
// no wording of a never/always/before kind is skipped without a model call.
func Compile(ctx context.Context, rules []Rule, llm *LLM, progress func(i, n int, r Rule)) []CompileResult {
	var out []CompileResult
	for i, r := range rules {
		if progress != nil {
			progress(i, len(rules), r)
		}
		res := CompileResult{Rule: r}
		det := Deterministic(r.Text)
		var model []Candidate
		if llm != nil && IsRuleText(r.Text) {
			var err error
			model, err = llm.Candidates(ctx, r.Text)
			res.LLMErr = err
		}
		res.Candidates = Merge(det, model)
		out = append(out, res)
	}
	return out
}

// RowsOf turns compile results into unmeasured rows (all suggestions).
func RowsOf(results []CompileResult) []Row {
	var rows []Row
	for _, cr := range results {
		for _, c := range cr.Candidates {
			rows = append(rows, Row{ID: ID(cr.Rule.Target, c.Spec), Target: cr.Rule.Target, RuleText: cr.Rule.Text,
				Spec: c.Spec, Source: c.Source, Why: c.Why, Status: StatusSuggestion, Reason: "not yet backtested"})
		}
	}
	return rows
}

// LoadCorpus reads the interactive transcript history of every agent with a
// profile, dropping headless, benchmark and automation sessions.
func LoadCorpus(home string, since time.Duration, agent string) (Corpus, int, []error) {
	profiles, errs := agentprofile.All(home)
	var kept []transcript.Session
	dropped := 0
	for _, p := range profiles {
		if p.Transcripts.Glob == "" || (agent != "" && agent != p.Name) {
			continue
		}
		opt := transcript.Options{NoAssistantText: true}
		if since > 0 {
			opt.Since = time.Now().Add(-since)
		}
		ss, e := transcript.ReadAll(transcript.Spec{Agent: p.Name, Format: p.Transcripts.Format, Glob: p.Transcripts.Glob, Map: p.Transcripts.Map}, opt)
		errs = append(errs, e...)
		for _, s := range ss {
			if impact.SessionRule(s) != "" {
				dropped++
				continue
			}
			kept = append(kept, s)
		}
	}
	return BuildCorpus(kept), dropped, errs
}

// BacktestOptions configures Measure.
type BacktestOptions struct {
	Corpus   Corpus
	Labeller *Labeller // nil = count matches only, no labels
	Policy   Policy
	// Retold returns how many times the outcome log recorded the user re-telling
	// the rule behind target; may be nil.
	Retold func(target string) int
}

// Measure backtests rows over the corpus, labels a sample of each rule's
// matches, and sets precision and status. Rows are updated in place.
func Measure(rows []Row, o BacktestOptions) map[string][]LabelledMatch {
	cs := make([]*Compiled, len(rows))
	keep := make([]int, 0, len(rows))
	for i := range rows {
		c, err := rows[i].Spec.Compile()
		if err != nil {
			rows[i].Reason = "invalid: " + err.Error()
			continue
		}
		cs[i] = c
		keep = append(keep, i)
	}
	var live []*Compiled
	for _, i := range keep {
		live = append(live, cs[i])
	}
	bts := Run(live, o.Corpus)
	perTarget := map[string][]int{}
	for j, i := range keep {
		b := bts[j]
		r := &rows[i]
		r.Scanned, r.Actions, r.Matches, r.Sessions, r.MatchRate = b.Scanned, b.Actions, b.Matches, b.Sessions, b.Rate
		r.Backtest = time.Now()
		r.Labelled, r.True, r.Sample, r.Unfaithful, r.FaithP = 0, 0, nil, false, 0
		if o.Retold != nil {
			r.Retold = o.Retold(r.Target)
		}
		perTarget[r.Target] = append(perTarget[r.Target], j)
	}
	labels := map[string][]LabelledMatch{}
	if o.Labeller != nil {
		for _, js := range perTarget {
			// Matches of a topic detector are not worth labels.
			var eligible []int
			for _, j := range js {
				if o.Policy.MaxMatchRate > 0 && rows[keep[j]].MatchRate > o.Policy.MaxMatchRate {
					continue
				}
				if bts[j].Matches > 0 {
					eligible = append(eligible, j)
				}
			}
			if len(eligible) == 0 {
				continue
			}
			var faithful []int
			for _, j := range eligible {
				r := &rows[keep[j]]
				p, ok := o.Labeller.Faithful(r.RuleText, r.Spec)
				r.FaithP, r.Unfaithful = p, ok && p < o.Policy.Faithful
				if !r.Unfaithful {
					faithful = append(faithful, j)
				}
			}
			if len(faithful) == 0 {
				continue
			}
			share := SamplePerRule / len(faithful)
			for _, j := range faithful {
				r := &rows[keep[j]]
				lm := o.Labeller.Label(r.RuleText, r.Spec, Pick(bts[j].Samples, share))
				r.Labelled, r.True = lm.N, lm.True
				labels[r.ID] = lm.Items
				r.Sample = lm.Items
			}
		}
	}
	for _, i := range keep {
		rows[i].Rescore(o.Policy)
	}
	return labels
}

// DefaultPaths are where rules state lives beside the other derived data.
func DefaultPaths(vaultRoot string) (dir, labelCache string) {
	dir = filepath.Join(vaultRoot, ".grimoire")
	return dir, filepath.Join(dir, "rules-labels.jsonl")
}

// HomeDir is $HOME, or "" when unknown.
func HomeDir() string { h, _ := os.UserHomeDir(); return h }
