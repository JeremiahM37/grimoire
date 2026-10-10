package api

import (
	"log"
	"math/rand/v2"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/rulecheck"
	"github.com/JeremiahM37/grimoire/go/internal/utilization"
)

// The utilization trace API and the holdout (docs/MEMORY_TRACE.md).

// maxHoldoutRate bounds memory_holdout_rate: above this the agent would be
// missing too many memories for an experiment to be a fair thing to run.
const maxHoldoutRate = 0.5

// defaultMinArm is the number of decision records each arm needs before the
// causal estimate is attempted.
const defaultMinArm = 10

func (s *Server) holdoutRate() float64 {
	v, err := strconv.ParseFloat(s.setting("memory_holdout_rate"), 64)
	if err != nil || v <= 0 {
		return 0
	}
	if v > maxHoldoutRate {
		return maxHoldoutRate
	}
	return v
}

func (s *Server) minArm() int {
	if n, err := strconv.Atoi(s.setting("memory_trace_min_arm")); err == nil && n > 0 {
		return n
	}
	return defaultMinArm
}

// holdoutProtected lists the targets the holdout must never withhold: rules
// with an enforce: ask check and every compiled check that is active. A
// 20-second cache keeps this off the per-request path.
func (s *Server) holdoutProtected() map[string]bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if s.holdProt != nil && time.Since(s.holdAt) < 20*time.Second {
		return s.holdProt
	}
	prot := map[string]bool{}
	if st := s.adh(); st != nil {
		if checks, err := st.Checks(); err == nil {
			for t, c := range checks {
				if c.Enforce == "ask" {
					prot[t] = true
				}
			}
		}
	}
	if rs, _ := s.rules(); rs != nil {
		if rows, err := rs.All(); err == nil {
			for _, r := range rows {
				if r.Status == rulecheck.StatusActive || r.Status == rulecheck.StatusEnforce {
					prot[r.Target] = true
				}
			}
		}
	}
	s.holdProt, s.holdAt = prot, time.Now()
	return prot
}

// holdout splits picked items into those shown and those withheld. An item is
// withheld with probability memory_holdout_rate unless it is pinned by a
// person, has an enforce: ask check or an active compiled check, or an enforce
// decision forced it into this very response. Shown items that faced the
// chance carry the probability (lg.pwith) so the log holds the propensity of
// both arms. With the rate at 0 this changes nothing.
func (s *Server) holdout(lg *ctxLog, picked []contextItem) (shown, held []contextItem) {
	rate := s.holdoutRate()
	if rate <= 0 {
		return picked, nil
	}
	draw := s.holdRand
	if draw == nil {
		draw = rand.Float64
	}
	prot := s.holdoutProtected()
	lg.pwith = map[string]float64{}
	for _, it := range picked {
		t := itemTarget(it)
		if it.pinned || prot[t] || lg.ask[t] {
			shown = append(shown, it)
			continue
		}
		if draw() < rate {
			held = append(held, it)
			continue
		}
		lg.pwith[it.Key] = rate
		shown = append(shown, it)
	}
	return shown, held
}

func (s *Server) logWithheld(lg ctxLog, held []contextItem, tagOf map[string]string) {
	st := s.adh()
	if st == nil {
		return
	}
	rate := s.holdoutRate()
	tool := ""
	if lg.stage == "action" {
		tool = lg.tool
	}
	rows := make([]adherence.Withheld, len(held))
	for i, it := range held {
		rows[i] = adherence.Withheld{Session: lg.session, Tag: tagOf[it.Key], Key: it.Key, Target: itemTarget(it),
			Kind: it.Kind, Stage: lg.stage, Tool: tool, Relevance: it.score, PWithhold: rate}
	}
	if err := st.LogWithheld(rows, time.Now()); err != nil {
		log.Printf("adherence: log withheld: %v", err)
	}
}

var tuRE = regexp.MustCompile(`^[A-Za-z0-9_\-:.]{1,128}$`)

// traceLog builds the logging context of a hybrid response: the situation, the
// permission decision, and for an action-stage request the pending call.
func (s *Server) traceLog(r *http.Request, query string, perm map[string]any, ask map[string]bool) ctxLog {
	q := r.URL.Query()
	lg := ctxLog{session: q.Get("session"), stage: q.Get("stage"), query: query, log: true, permission: perm, ask: ask}
	if lg.stage == "action" {
		lg.tool, lg.pend = "", ""
		if tool, target := actionParts(r, query); tool != "" && strings.TrimSpace(target) != "" {
			lg.tool, lg.pend = tool, adherence.PendingHash(tool, target)
		}
		if tu := q.Get("tu"); tuRE.MatchString(tu) {
			lg.tu = tu
		}
	}
	return lg
}

// tracePrompt marks a prompt boundary for the session. A prompt that restates
// something the agent was told before (the same pattern the cue learner uses)
// marks the previous turn's actions corrected.
func (s *Server) tracePrompt(session string, corrected bool) {
	if session == "" || !sessionRE.MatchString(session) {
		return
	}
	if st := s.adh(); st != nil {
		if _, err := st.TracePrompt(session, corrected, time.Now()); err != nil {
			log.Printf("adherence: trace prompt: %v", err)
		}
	}
}

// traceFields are the outcome-hook fields the trace adds to /api/memory/outcome.
// Only codes, counts, ids and hashes: no command, path or output text.
type traceFields struct {
	TU     string `json:"tu"`     // the harness's tool_use id
	Region string `json:"region"` // id of the content this call produced
	// Err is 1 when the call failed (tool error or non-zero exit), 0 when it
	// succeeded; absent when the hook could not tell.
	Err      *int `json:"err"`
	Exit     *int `json:"exit"`
	TestPass *int `json:"tp"`
	TestFail *int `json:"tf"`
	Thrash   int  `json:"thrash"`
	Denied   bool `json:"denied"`
	// Reedit and Revert list regions of earlier calls this call edited again
	// or undid; the outcome belongs to those earlier calls.
	Reedit []string `json:"reedit"`
	Revert []string `json:"revert"`
	// Ev is {tag: fingerprints matched in THIS call}, unlike fp (cumulative).
	Ev      map[string]int     `json:"ev"`
	Meaning map[string]float64 `json:"meaning"`
	// Prompt marks a prompt event (no tool); Corr is the hook's local verdict
	// that the prompt corrects the agent.
	Prompt bool `json:"prompt"`
	Corr   bool `json:"corr"`
}

var regionRE = regexp.MustCompile(`^[0-9a-f]{8,32}$`)

func (f traceFields) valid() bool {
	if (f.TU != "" && !tuRE.MatchString(f.TU)) || (f.Region != "" && !regionRE.MatchString(f.Region)) ||
		len(f.Reedit) > 8 || len(f.Revert) > 8 || len(f.Ev) > 16 || len(f.Meaning) > 16 || f.Thrash < 0 || f.Thrash > 1000 {
		return false
	}
	for _, r := range append(append([]string{}, f.Reedit...), f.Revert...) {
		if !regionRE.MatchString(r) {
			return false
		}
	}
	for t, n := range f.Ev {
		if !tagRE.MatchString(t) || n < 0 || n > 6 {
			return false
		}
	}
	for t, v := range f.Meaning {
		if !tagRE.MatchString(t) || v < 0 || v > 1 {
			return false
		}
	}
	for _, p := range []*int{f.Err, f.TestPass, f.TestFail} {
		if p != nil && (*p < 0 || *p > 1_000_000) {
			return false
		}
	}
	return f.Err == nil || *f.Err <= 1
}

// recordTrace stores an executed call and links the memories it carried
// evidence of. A require check that the call satisfied is evidence too.
func (s *Server) recordTrace(st *adherence.Store, in outcomeIn, rows []adherence.Row, checks map[string]adherence.Check) {
	f := in.traceFields
	a := adherence.ActionIn{Session: in.Session, TU: f.TU, Tool: in.Tool, Target: in.Target, Region: f.Region,
		Failed: -1, ExitCode: f.Exit, TestsPass: -1, TestsFail: -1, Thrash: f.Thrash, Denied: f.Denied,
		Reedit: f.Reedit, Revert: f.Revert, Ev: f.Ev, Cited: in.Cited, TS: time.Now()}
	if f.Err != nil {
		a.Failed = *f.Err
	} else if f.Exit != nil {
		a.Failed = b2i(*f.Exit != 0)
	}
	if f.TestPass != nil {
		a.TestsPass = *f.TestPass
	}
	if f.TestFail != nil {
		a.TestsFail = *f.TestFail
	}
	id, _, err := st.RecordAction(a)
	if err != nil {
		log.Printf("adherence: trace action: %v", err)
		return
	}
	for _, row := range rows {
		if c, ok := checks[row.Target]; ok && c.AppliesTo(in.Tool) && c.Require != "" && c.Satisfies(in.Target) {
			st.Link(row.ID, id, adherence.EvCheck, time.Now())
		}
	}
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- API ----

type causalView struct {
	// Label is "caused" only when a randomised estimate backs the number.
	Label     string                `json:"label"`
	Status    string                `json:"status"`
	Rate      float64               `json:"holdout_rate"`
	Treated   int                   `json:"treated"`
	Withheld  int                   `json:"withheld"`
	MinArm    int                   `json:"min_per_arm"`
	Estimator string                `json:"estimator,omitempty"`
	Estimate  *utilization.Estimate `json:"estimate,omitempty"`
	Note      string                `json:"note,omitempty"`
}

func (s *Server) estimator() utilization.Estimator {
	if s.Estimator == nil {
		return utilization.None{}
	}
	return s.Estimator
}

// causal turns decision records into the causal view, or says why it cannot.
func (s *Server) causal(recs []utilization.Record) causalView {
	v := causalView{Label: "", Rate: s.holdoutRate(), MinArm: s.minArm()}
	for _, r := range recs {
		if r.Treated {
			v.Treated++
		} else {
			v.Withheld++
		}
	}
	switch {
	case v.Rate == 0 && v.Withheld == 0:
		v.Status = "holdout off"
		v.Note = "set memory_holdout_rate above 0 to randomise injections and measure a causal effect"
	case v.Treated < v.MinArm || v.Withheld < v.MinArm:
		v.Status = "insufficient data"
		v.Note = "needs at least " + strconv.Itoa(v.MinArm) + " shown and " + strconv.Itoa(v.MinArm) + " withheld decisions with observable outcomes"
	default:
		est := s.estimator()
		v.Estimator = est.Name()
		e, err := est.Lift(recs)
		switch {
		case err == utilization.ErrNoEstimator:
			v.Status = "estimator not installed"
		case err != nil:
			v.Status = "estimator error"
			v.Note = err.Error()
		default:
			v.Status, v.Label, v.Estimate = "estimated", "caused", &e
		}
	}
	return v
}

func traceDays(r *http.Request) (int, time.Time) {
	days := clampLimit(r.URL.Query().Get("days"), 30, 30)
	return days, time.Now().Add(-time.Duration(days) * 24 * time.Hour)
}

// traceCard serves one memory's trace.
func (s *Server) traceCard(w http.ResponseWriter, r *http.Request) {
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	target := r.URL.Query().Get("target")
	if !strings.HasPrefix(target, "fact:") && !strings.HasPrefix(target, "note:") {
		writeErr(w, http.StatusBadRequest, "target must be fact:<id> or note:<path>")
		return
	}
	days, since := traceDays(r)
	card, err := st.Card(target, since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if card.Exposures == 0 && card.Withheld == 0 {
		writeErr(w, http.StatusNotFound, "no trace for that target in the window")
		return
	}
	if card.Path != "" {
		if !s.canRead(r, card.Path) {
			writeErr(w, http.StatusNotFound, "no trace for that target in the window")
			return
		}
	} else if it, ok := s.cueTargetItem(r, target, nil); !ok || !s.canRead(r, it.Path) {
		writeErr(w, http.StatusNotFound, "no trace for that target in the window")
		return
	}
	recs, err := st.Records(target, since, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "card": card, "causal": s.causal(recs),
		"labels": traceLabels})
}

var traceLabels = map[string]string{
	"benefit": "associated: influenced actions compared with comparable actions that were not; it does not show the memory caused the difference",
	"causal":  "caused: only from the randomised holdout, with an interval",
}

// traceSummary rolls the trace up by kind and overall.
func (s *Server) traceSummary(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	days, since := traceDays(r)
	sum, err := st.Summary(since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recs, err := st.Records("", since, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byKind := map[string][]utilization.Record{}
	for _, rec := range recs {
		byKind[rec.Kind] = append(byKind[rec.Kind], rec)
	}
	type row struct {
		adherence.KindSummary
		Causal causalView `json:"causal"`
	}
	out := make([]row, 0, len(sum))
	for _, k := range sum {
		in := recs
		if k.Kind != "" {
			in = byKind[k.Kind]
		}
		out = append(out, row{k, s.causal(in)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "holdout_rate": s.holdoutRate(), "kinds": out, "labels": traceLabels})
}
