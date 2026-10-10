package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
)

// Rules compiled into checks (docs/MEMORY_RULES.md). State lives in rules.db
// beside the adherence log; the evaluator reads only active checks.

var rulesState = struct {
	sync.Mutex
	store *rulecheck.Store
	live  *rulecheck.Live
	root  string
}{}

func (s *Server) rules() (*rulecheck.Store, *rulecheck.Live) {
	if s.Vault == nil {
		return nil, nil
	}
	rulesState.Lock()
	defer rulesState.Unlock()
	if rulesState.store != nil && rulesState.root == s.Vault.Root {
		return rulesState.store, rulesState.live
	}
	st, err := rulecheck.Open(filepath.Join(s.Vault.Root, ".grimoire"))
	if err != nil {
		log.Printf("rules: %v", err)
		return nil, nil
	}
	rulesState.store, rulesState.root = st, s.Vault.Root
	rulesState.live = &rulecheck.Live{Store: st, Policy: s.rulesPolicy}
	return st, rulesState.live
}

func (s *Server) ruleFloat(key string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s.setting(key)), 64); err == nil && v >= 0 && v <= 1 {
		return v
	}
	return def
}

func (s *Server) rulesPolicy() rulecheck.Policy {
	p := rulecheck.DefaultPolicy()
	p.MinPrecision = s.ruleFloat("rules_min_precision", p.MinPrecision)
	p.EnforcePrecision = s.ruleFloat("rules_enforce_precision", p.EnforcePrecision)
	p.MaxMatchRate = s.ruleFloat("rules_max_match_rate", p.MaxMatchRate)
	p.Faithful = s.ruleFloat("rules_faithful_threshold", p.Faithful)
	if n, err := strconv.Atoi(strings.TrimSpace(s.setting("rules_min_labelled"))); err == nil && n > 0 {
		p.MinLabelled = n
	}
	return p
}

// rulesLabelClient is the model that labels backtest matches: rules_label_*,
// falling back to the decision model.
func (s *Server) rulesLabelClient() *decide.Client {
	url := strings.TrimSpace(s.setting("rules_label_url"))
	if url == "" {
		return s.decisionClient()
	}
	key := strings.TrimSpace(s.setting("rules_label_api_key"))
	if key == "" {
		key = strings.TrimSpace(s.setting("decision_api_key"))
	}
	model := strings.TrimSpace(s.setting("rules_label_model"))
	if model == "" {
		model = "jev-latest"
	}
	return &decide.Client{BaseURL: url, Model: model, APIKey: key, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

func (s *Server) rulesLLM() *rulecheck.LLM {
	url := strings.TrimSpace(s.setting("rules_llm_url"))
	if url == "" {
		return nil
	}
	l := rulecheck.DefaultLLM(url)
	if m := strings.TrimSpace(s.setting("rules_llm_model")); m != "" {
		l.Model = m
	}
	return l
}

func (s *Server) rulesDir(q map[string]string) (dir, prefix string) {
	dir = q["dir"]
	if dir == "" {
		dir = s.canonicalMemoryDir()
	} else {
		dir = expandHome(dir)
	}
	prefix = q["prefix"]
	if prefix == "" {
		prefix = "Agent Memory/"
	}
	return dir, prefix
}

func (s *Server) rulesAdmin(w http.ResponseWriter, r *http.Request) (*rulecheck.Store, *rulecheck.Live, bool) {
	if p := principal(r); !(p.Unrestricted || p.IsAdmin()) {
		writeErr(w, http.StatusForbidden, "rules read transcripts and change what agents are asked; administrators only")
		return nil, nil, false
	}
	st, live := s.rules()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "rules store unavailable")
		return nil, nil, false
	}
	return st, live, true
}

// rulesList: GET /api/memory/rules[?status=active]
func (s *Server) rulesList(w http.ResponseWriter, r *http.Request) {
	st, _, ok := s.rulesAdmin(w, r)
	if !ok {
		return
	}
	rows, _ := st.All()
	want := r.URL.Query().Get("status")
	out := []rulecheck.Row{}
	for _, row := range rows {
		if want == "" || row.Status == want {
			out = append(out, row)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out, "policy": s.rulesPolicy()})
}

// rulesCompile: POST /api/memory/rules/compile {dir?, prefix?, llm?: bool}
func (s *Server) rulesCompile(w http.ResponseWriter, r *http.Request) {
	st, live, ok := s.rulesAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Dir    string `json:"dir"`
		Prefix string `json:"prefix"`
		LLM    *bool  `json:"llm"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
	dir, prefix := s.rulesDir(map[string]string{"dir": in.Dir, "prefix": in.Prefix})
	if dir == "" {
		writeErr(w, http.StatusBadRequest, "no memory store: set memory_canonical_dir or pass dir")
		return
	}
	rules, err := rulecheck.RulesFromStore(dir, prefix)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	var llm *rulecheck.LLM
	if in.LLM == nil || *in.LLM {
		llm = s.rulesLLM()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Hour)
	defer cancel()
	results := rulecheck.Compile(ctx, rules, llm, nil)
	policy := s.rulesPolicy()
	byTarget := map[string][]rulecheck.Row{}
	rows := rulecheck.RowsOf(results)
	for _, row := range rows {
		byTarget[row.Target] = append(byTarget[row.Target], row)
	}
	// Existing measurements survive a recompile of an unchanged check.
	existing, _ := st.All()
	prev := map[string]rulecheck.Row{}
	for _, e := range existing {
		prev[e.ID] = e
	}
	noCheck, llmErrs, det, mod := 0, 0, 0, 0
	for _, cr := range results {
		if len(cr.Candidates) == 0 {
			noCheck++
		}
		if cr.LLMErr != nil {
			llmErrs++
		}
		for _, c := range cr.Candidates {
			if c.Source == "llm" {
				mod++
			} else {
				det++
			}
		}
	}
	for t, rs := range byTarget {
		for i := range rs {
			if o, ok := prev[rs[i].ID]; ok {
				rs[i] = o
				rs[i].RuleText = rulesText(results, t, rs[i].RuleText)
			}
		}
		if err := st.ReplaceTarget(t, rs, policy); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	live.Invalidate()
	writeJSON(w, http.StatusOK, map[string]any{"rules": len(rules), "checks": len(rows), "rules_without_check": noCheck,
		"deterministic": det, "llm": mod, "llm_errors": llmErrs, "llm_used": llm != nil})
}

func rulesText(rs []rulecheck.CompileResult, target, def string) string {
	for _, r := range rs {
		if r.Rule.Target == target {
			return r.Rule.Text
		}
	}
	return def
}

// rulesBacktest: POST /api/memory/rules/backtest {since?, agent?, label?: bool, budget?}
func (s *Server) rulesBacktest(w http.ResponseWriter, r *http.Request) {
	st, live, ok := s.rulesAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Since  string `json:"since"`
		Agent  string `json:"agent"`
		Label  *bool  `json:"label"`
		Budget int    `json:"budget"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in)
	since, err := ParseSince(in.Since)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, _ := st.All()
	if len(rows) == 0 {
		writeErr(w, http.StatusBadRequest, "no compiled checks: run rules compile first")
		return
	}
	corpus, dropped, errs := rulecheck.LoadCorpus(home, since, in.Agent)
	policy := s.rulesPolicy()
	opt := rulecheck.BacktestOptions{Corpus: corpus, Policy: policy}
	if a := s.adh(); a != nil {
		by, _, _ := a.Report(time.Now().Add(-adherence.Retention))
		opt.Retold = func(target string) int {
			if st := by[target]; st != nil {
				return st.Contradicted
			}
			return 0
		}
	}
	var lab *rulecheck.Labeller
	if in.Label == nil || *in.Label {
		if c := s.rulesLabelClient(); c != nil {
			budget := in.Budget
			if budget <= 0 {
				budget, _ = strconv.Atoi(s.setting("rules_label_budget"))
			}
			if budget <= 0 {
				budget = 1500
			}
			_, cache := rulecheck.DefaultPaths(s.Vault.Root)
			lab = &rulecheck.Labeller{Client: c, Threshold: s.ruleFloat("rules_label_threshold", 0.5), Budget: budget, CachePath: cache}
			opt.Labeller = lab
		}
	}
	rulecheck.Measure(rows, opt)
	byTarget := map[string][]rulecheck.Row{}
	for _, row := range rows {
		byTarget[row.Target] = append(byTarget[row.Target], row)
	}
	for t, rs := range byTarget {
		if err := st.ReplaceTarget(t, rs, policy); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	live.Invalidate()
	resp := map[string]any{"checks": len(rows), "sessions": len(corpus.Sessions), "tool_calls": corpus.Calls,
		"sessions_excluded": dropped, "labelled_with_model": lab != nil}
	if lab != nil {
		resp["label_calls"], resp["label_cache_hits"], resp["label_errors"] = lab.Calls, lab.Hits, lab.Errs
	}
	for i, e := range errs {
		if i == 5 {
			break
		}
		resp["note_"+strconv.Itoa(i)] = e.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// rulesSet: POST /api/memory/rules/{enable|disable} {id, enforce?}
func (s *Server) rulesSet(state func(enforce bool) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, live, ok := s.rulesAdmin(w, r)
		if !ok {
			return
		}
		var in struct {
			ID      string `json:"id"`
			Enforce bool   `json:"enforce"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.ID == "" {
			writeErr(w, http.StatusBadRequest, "body must be JSON {id, enforce?}")
			return
		}
		row, err := st.SetUser(in.ID, state(in.Enforce), s.rulesPolicy())
		if err != nil {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		live.Invalidate()
		writeJSON(w, http.StatusOK, row)
	}
}

// rulesLabel: POST /api/memory/rules/label {firing, violation: bool}. A live
// firing judged by a person; it folds into precision and can demote the check.
func (s *Server) rulesLabel(w http.ResponseWriter, r *http.Request) {
	st, live, ok := s.rulesAdmin(w, r)
	if !ok {
		return
	}
	var in struct {
		Firing    int64 `json:"firing"`
		Violation bool  `json:"violation"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil || in.Firing == 0 {
		writeErr(w, http.StatusBadRequest, "body must be JSON {firing, violation}")
		return
	}
	row, err := st.LabelFiring(in.Firing, in.Violation, s.rulesPolicy())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	live.Invalidate()
	writeJSON(w, http.StatusOK, row)
}

// actionContext is who is acting, from an outcome body or action-stage query.
type actionContext struct{ session, tool, target, cwd, agent string }

func actionFromQuery(r *http.Request, tool, target string) actionContext {
	q := r.URL.Query()
	return actionContext{session: q.Get("session"), tool: tool, target: target, cwd: q.Get("cwd"), agent: q.Get("agent")}
}

// ruleHits finds the active compiled checks a live call trips, as injected
// items plus a permission decision when any of them enforces.
func (s *Server) ruleHits(r *http.Request, a actionContext) ([]contextItem, map[string]any) {
	_, live := s.rules()
	if live == nil || a.tool == "" || a.target == "" {
		return nil, nil
	}
	hits := live.Check(a.session, a.tool, a.target, a.cwd, a.agent)
	if len(hits) == 0 {
		return nil, nil
	}
	var items []contextItem
	var reasons []string
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.Row.Target] {
			continue
		}
		seen[h.Row.Target] = true
		if it, ok := s.cueTargetItem(r, h.Row.Target, nil); ok {
			it.Key = itemKey(it)
			items = append(items, it)
		}
		if h.Enforce {
			reasons = append(reasons, "Grimoire rule: "+clipText(strings.Join(strings.Fields(h.Row.RuleText), " "), 240))
		}
	}
	if len(reasons) == 0 {
		return items, nil
	}
	return items, map[string]any{"decision": "ask", "reason": strings.Join(reasons, " | ") +
		fmt.Sprintf(" (a compiled check measured at precision >= %.2f flags this action; allow only if the user wants it)", s.rulesPolicy().MinPrecision)}
}

// ruleObserve is the PostToolUse side: count firings, mark the injected rule
// violated, remember the call for require_before.
func (s *Server) ruleObserve(a actionContext, st *adherence.Store, rows []adherence.Row) {
	_, live := s.rules()
	if live == nil || a.tool == "" || a.target == "" {
		return
	}
	hits := live.Check(a.session, a.tool, a.target, a.cwd, a.agent)
	live.Observe(a.session, a.tool, a.target, a.cwd, a.agent)
	if st == nil {
		return
	}
	for _, h := range hits {
		for _, row := range rows {
			if row.Target == h.Row.Target {
				st.SetCheck(row.ID, adherence.Violated)
			}
		}
	}
}

// rulesRetold: the user had to say a rule again after a check fired on it, so
// the newest firing was a real violation.
func (s *Server) rulesRetold(target string) {
	st, live := s.rules()
	if st == nil {
		return
	}
	if id, ok := st.NewestOpenFiring(target, outcomeWindow, time.Now()); ok {
		if _, err := st.LabelFiring(id, true, s.rulesPolicy()); err == nil {
			live.Invalidate()
		}
	}
}
