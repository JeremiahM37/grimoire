package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/decide"
	fpr "github.com/JeremiahM37/grimoire/go/internal/fingerprint"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// outcomeWindow is how far back an injection is still attributed to the
// activity that follows it.
const outcomeWindow = 6 * time.Hour

// adh opens the adherence store on first use. A store that cannot open leaves
// injection exactly as it was without adherence tracking.
func (s *Server) adh() *adherence.Store {
	s.adhOnce.Do(func() {
		if s.Vault == nil || s.Index == nil {
			return
		}
		st, err := adherence.Open(filepath.Join(s.Vault.Root, ".grimoire"))
		if err != nil {
			log.Printf("adherence: %v", err)
			return
		}
		s.adhStore = st
	})
	return s.adhStore
}

func itemTarget(it contextItem) string {
	if it.ID != "" {
		return cues.FactTarget(it.ID)
	}
	return cues.NoteTarget(it.Path)
}

// logInjections records what a response injected.
func (s *Server) logInjections(lg ctxLog, picked []contextItem, tags []string, fps [][]fpr.Fingerprint) {
	st := s.adh()
	if st == nil {
		return
	}
	rows := make([]adherence.Injection, len(picked))
	for i, it := range picked {
		rows[i] = adherence.Injection{Session: lg.session, Tag: tags[i], Key: it.Key, Target: itemTarget(it),
			Path: it.Path, FactID: it.ID, Stage: lg.stage, Relevance: it.score}
		if fps != nil {
			rows[i].FPKnown, rows[i].FPN = true, len(fps[i])
		}
	}
	if err := st.Log(rows, time.Now()); err != nil {
		log.Printf("adherence: log: %v", err)
	}
}

// adherenceFilter applies, in order: action-time enforcement (rules that ask
// before a forbidden action, injected whatever their relevance), the optional
// decision-model gate, and the down-rank of memories agents keep ignoring. The
// down-rank is for injection only; recall and search are untouched.
func (s *Server) adherenceFilter(r *http.Request, query string, items []contextItem, minRel float64) ([]contextItem, map[string]any) {
	var perm map[string]any
	if r.URL.Query().Get("stage") == "action" {
		tool, target := actionParts(r, query)
		var forced []contextItem
		forced, perm = s.enforceFor(r, tool, target)
		for _, f := range forced {
			f.score = 1
			replaced := false
			for i := range items {
				if itemTarget(items[i]) == itemTarget(f) {
					items[i].score, replaced = 1, true
				}
			}
			if !replaced {
				items = append(items, f)
			}
		}
	}
	items = s.applyGate(r.Context(), query, items, minRel)
	if st := s.adh(); st != nil {
		targets := make([]string, 0, len(items))
		for _, it := range items {
			targets = append(targets, itemTarget(it))
		}
		pen := st.IgnoredPenalty(targets, time.Now().Add(-adherence.Retention))
		for i := range items {
			if p, ok := pen[itemTarget(items[i])]; ok && items[i].score < 1 {
				items[i].score *= p
			}
		}
	}
	return items, perm
}

// actionParts splits an action-stage query, "Bash git push", into tool and
// target. Explicit tool/target parameters win.
func actionParts(r *http.Request, query string) (string, string) {
	q := r.URL.Query()
	if t := q.Get("tool"); t != "" {
		return t, q.Get("target")
	}
	tool, target, _ := strings.Cut(strings.TrimSpace(query), " ")
	return tool, target
}

// enforceFor finds the rules whose `enforce: ask` check forbids this action,
// and the permission decision to return for them. It is deterministic: no
// embedding, no model, and it applies whatever the rule's relevance score.
func (s *Server) enforceFor(r *http.Request, tool, target string) ([]contextItem, map[string]any) {
	st := s.adh()
	if st == nil || tool == "" || target == "" {
		return nil, nil
	}
	checks, err := st.Checks()
	if err != nil {
		return nil, nil
	}
	var items []contextItem
	var reasons []string
	for _, c := range checks {
		if c.Enforce != "ask" || !c.AppliesTo(tool) || !c.Violates(target) {
			continue
		}
		it, ok := s.cueTargetItem(r, c.Target, nil)
		if !ok {
			continue
		}
		it.Key = itemKey(it)
		items = append(items, it)
		reasons = append(reasons, "Grimoire rule: "+clipText(strings.Join(strings.Fields(it.Text), " "), 240))
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items, map[string]any{"decision": "ask", "reason": strings.Join(reasons, " | ") +
		" (this action matches the rule's forbid pattern; allow only if the user wants it)"}
}

func clipText(t string, n int) string {
	if r := []rune(t); len(r) > n {
		return string(r[:n]) + "…"
	}
	return t
}

type outcomeIn struct {
	Session string   `json:"session"`
	Tool    string   `json:"tool"`
	Target  string   `json:"target"`
	Cited   []string `json:"cited"`
	// FP is {tag: fingerprints matched} computed by the hook from salted
	// hashes; the server never sees what matched, or any text.
	FP   map[string]int `json:"fp"`
	Stop bool           `json:"stop"`
	Pre  bool           `json:"pre"` // only compute the permission decision; record nothing
}

var tagRE = regexp.MustCompile(`^[0-9a-f]{4,8}$`)

var sessionRE = regexp.MustCompile(`^[A-Za-z0-9_\-]{8,128}$`)

// memoryOutcome takes what an agent did after being shown memories and
// classifies each injected item: cited, followed (its check passed),
// violated (its check forbade what happened) or ignored. The hook sends tool
// names and targets and the memory tags the agent cited; never transcript
// text.
func (s *Server) memoryOutcome(w http.ResponseWriter, r *http.Request) {
	var in outcomeIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON {session, tool, target} or {session, cited, stop}")
		return
	}
	if !sessionRE.MatchString(in.Session) || len(in.Target) > 4000 || len(in.Tool) > 64 || len(in.Cited) > 32 {
		writeErr(w, http.StatusBadRequest, "invalid session, tool, target or cited")
		return
	}
	for _, t := range in.Cited {
		if !tagRE.MatchString(t) {
			writeErr(w, http.StatusBadRequest, "cited tags are 4-8 hex characters")
			return
		}
	}
	if len(in.FP) > 16 {
		writeErr(w, http.StatusBadRequest, "fp has at most 16 tags")
		return
	}
	for t, n := range in.FP {
		if !tagRE.MatchString(t) || n < 1 || n > fpr.MaxPerMemory {
			writeErr(w, http.StatusBadRequest, "fp is {tag: 1..6}")
			return
		}
	}
	if in.Pre {
		_, perm := s.enforceFor(r, in.Tool, in.Target)
		writeJSON(w, http.StatusOK, map[string]any{"permission": perm})
		return
	}
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	since := time.Now().Add(-outcomeWindow)
	resp := map[string]any{"ok": true}
	if in.Tool != "" {
		rows, err := st.OpenRows(in.Session, since)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		checks, _ := st.Checks()
		for _, row := range rows {
			c, ok := checks[row.Target]
			if !ok || !c.AppliesTo(in.Tool) {
				continue
			}
			switch {
			case c.Violates(in.Target):
				st.SetCheck(row.ID, adherence.Violated)
			case c.Require != "" && c.Satisfies(in.Target):
				st.SetCheck(row.ID, adherence.Followed)
			}
		}
		st.BumpTools(in.Session, since)
	}
	if len(in.Cited) > 0 {
		n, _ := st.MarkCited(in.Session, in.Cited, since)
		resp["cited"] = n
	}
	if len(in.FP) > 0 {
		resp["fp"] = s.recordFingerprintCounts(r, st, in.Session, in.FP, since)
	}
	if in.Stop {
		rows, err := st.Finalize(in.Session, since)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		checks, _ := st.Checks()
		outcomes := map[string]string{}
		for _, row := range rows {
			// A check that never fired across a turn with tool calls held
			// (forbid) or was never met (require) is settled here.
			if c, ok := checks[row.Target]; ok && row.CheckResult == "" && row.Tools > 0 {
				if c.Require != "" {
					row.CheckResult = adherence.Violated
					st.SetCheck(row.ID, adherence.Violated)
				} else if c.Forbid != "" {
					row.CheckResult = adherence.Followed
					st.SetCheck(row.ID, adherence.Followed)
				}
			}
			outcomes[row.Tag] = row.Outcome()
			s.applyOutcome(r, row)
		}
		resp["outcomes"] = outcomes
	}
	writeJSON(w, http.StatusOK, resp)
}

// applyOutcome feeds a finished injection into the memory's feedback counters:
// acted-on (cited, followed or used) counts as helpful. Ignored and violated move no
// counter; ignored injections are down-ranked for injection only, and a
// violation says the agent erred, not that the memory is wrong. The counter
// changes only for facts the caller may write, like POST /api/memory/feedback.
func (s *Server) applyOutcome(r *http.Request, row adherence.Row) {
	if row.Counted || row.FactID == "" || row.Path == "" {
		return
	}
	o := row.Outcome()
	if o != adherence.Cited && o != adherence.Followed && o != adherence.Used {
		return
	}
	if r != nil && !s.canWrite(r, row.Path) {
		return
	}
	if err := s.mutateEntry(row.Path, row.FactID, func(e *memory.Entry) { e.Helpful++ }); err != nil {
		return
	}
	if st := s.adh(); st != nil {
		st.MarkCounted(row.ID)
	}
}

// adherenceRetold is called when a later prompt or remember shows the user had
// to tell the agent something the memory already held. The newest injection of
// that memory is marked contradicted, and the memory's unhelpful counter moves
// once: it was in front of the agent and did not stick.
func (s *Server) adherenceRetold(target string) {
	st := s.adh()
	if st == nil || target == "" {
		return
	}
	row, err := st.Contradict(target, outcomeWindow, time.Now())
	if err != nil || row == nil || row.FactID == "" || row.Path == "" {
		return
	}
	s.mutateEntry(row.Path, row.FactID, func(e *memory.Entry) { e.Unhelpful++ })
}

// adherenceReport shows per-memory and overall rates.
func (s *Server) adherenceReport(w http.ResponseWriter, r *http.Request) {
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	days := clampLimit(r.URL.Query().Get("days"), 30, 30)
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	by, all, err := st.Report(since)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type item struct {
		Target       string  `json:"target"`
		Injected     int     `json:"injected"`
		Cited        int     `json:"cited"`
		Followed     int     `json:"followed"`
		Used         int     `json:"used"`
		Violated     int     `json:"violated"`
		Ignored      int     `json:"ignored"`
		Unknown      int     `json:"unknown"`
		Contradicted int     `json:"contradicted"`
		Pending      int     `json:"pending"`
		Rate         float64 `json:"rate"`
		UsedRate     float64 `json:"used_rate"`
		Coverage     float64 `json:"fingerprint_coverage"`
		Penalty      float64 `json:"injection_penalty,omitempty"`
	}
	list := []item{}
	for _, v := range by {
		// A reader sees only memories they may read.
		if v.Path != "" && !s.canRead(r, v.Path) {
			continue
		}
		it := item{Target: v.Target, Injected: v.Injected, Cited: v.Cited, Followed: v.Followed, Used: v.Used,
			Violated: v.Violated, Ignored: v.Ignored, Unknown: v.Unknown, Contradicted: v.Contradicted,
			Pending: v.Pending, Rate: v.Rate(), UsedRate: v.UsedRate(), Coverage: v.Coverage()}
		if p := adherence.Penalty(*v); p < 1 {
			it.Penalty = p
		}
		list = append(list, it)
	}
	sort.Slice(list, func(a, b int) bool { return list[a].Injected > list[b].Injected })
	gate, _ := st.Gate(since)
	writeJSON(w, http.StatusOK, map[string]any{"days": days, "overall": map[string]any{
		"injected": all.Injected, "cited": all.Cited, "followed": all.Followed, "used": all.Used, "violated": all.Violated,
		"ignored": all.Ignored, "unknown": all.Unknown, "contradicted": all.Contradicted, "pending": all.Pending,
		"rate": all.Rate(), "used_rate": all.UsedRate(), "fingerprint_coverage": all.Coverage()},
		"memories": list, "gate": gate})
}

// ---- checks ----

type checkIn struct {
	Target  string   `json:"target"`
	Forbid  string   `json:"forbid"`
	Require string   `json:"require"`
	Enforce string   `json:"enforce"`
	Tools   []string `json:"tools"`
}

func (s *Server) checkTarget(w http.ResponseWriter, r *http.Request, target string) bool {
	if !strings.HasPrefix(target, "fact:") && !strings.HasPrefix(target, "note:") {
		writeErr(w, http.StatusBadRequest, "target must be fact:<id> or note:<path>")
		return false
	}
	it, ok := s.cueTargetItem(r, target, nil)
	if !ok {
		writeErr(w, http.StatusNotFound, "no current fact or readable note by that target")
		return false
	}
	if !s.requireWrite(w, r, it.Path) {
		return false
	}
	return true
}

// setCheck sets a hand-written check on a memory. It takes write access to
// the memory, since an `enforce: ask` check changes what the agent is stopped
// from doing.
func (s *Server) setCheck(w http.ResponseWriter, r *http.Request) {
	var in checkIn
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON {target, forbid|require, enforce?, tools?}")
		return
	}
	if !s.checkTarget(w, r, in.Target) {
		return
	}
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	c := adherence.Check{Target: in.Target, Forbid: in.Forbid, Require: in.Require, Enforce: in.Enforce, Tools: in.Tools, Source: "hand"}
	if err := st.UpsertCheck(c, time.Now()); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st.DeleteSuggestion(in.Target)
	writeJSON(w, http.StatusOK, c)
}

func (s *Server) deleteCheck(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Query().Get("target")
	if !s.checkTarget(w, r, target) {
		return
	}
	if st := s.adh(); st != nil {
		st.DeleteCheck(target)
		st.DeleteSuggestion(target)
	}
	writeJSON(w, http.StatusOK, map[string]any{"target": target, "deleted": true})
}

// listChecks shows the checks (or, with suggestions=1, the proposals awaiting
// a person) for memories the caller may read.
func (s *Server) listChecks(w http.ResponseWriter, r *http.Request) {
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	visible := func(target string) bool {
		it, ok := s.cueTargetItem(r, target, nil)
		return ok && s.canRead(r, it.Path)
	}
	if r.URL.Query().Get("suggestions") == "1" {
		all, _ := st.Suggestions()
		out := []adherence.Suggestion{}
		for _, g := range all {
			if visible(g.Target) {
				out = append(out, g)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"suggestions": out})
		return
	}
	all, _ := st.Checks()
	out := []adherence.Check{}
	for t, c := range all {
		if visible(t) {
			out = append(out, c)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"checks": out})
}

// acceptCheck turns a proposal into a check. Only a person does this; a
// proposal never takes effect on its own. With enforce set it can also turn
// the check into an ask-before-acting rule.
func (s *Server) acceptCheck(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Target  string `json:"target"`
		Enforce string `json:"enforce"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "body must be JSON {target, enforce?}")
		return
	}
	if !s.checkTarget(w, r, in.Target) {
		return
	}
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	all, _ := st.Suggestions()
	for _, g := range all {
		if g.Target != in.Target {
			continue
		}
		c := g.Check
		c.Enforce = in.Enforce
		c.Source = "accepted"
		if err := st.UpsertCheck(c, time.Now()); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		st.DeleteSuggestion(in.Target)
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeErr(w, http.StatusNotFound, "no proposal for that target")
}

var checkQuestion = map[string]decide.Question{"detector": {
	Type: decide.Noul,
	Instructions: "A standing rule for a coding agent is given, with a regular expression. Would a tool call " +
		"(a shell command or file path) that matches the expression be breaking the rule? Answer yes only if " +
		"matching the expression means the rule is broken, not merely that the topic is related.",
	Criteria: map[string]string{
		"true":  "a command matching the expression breaks the rule",
		"false": "the expression would match harmless commands or miss the point of the rule",
	},
}}

// proposeThreshold is the model probability a pattern needs to be proposed.
const proposeThreshold = 0.7

// proposeChecks is the offline pass: for rule memories with no check, derive
// candidate patterns from the rule's text, ask the configured decision model
// whether each is a faithful detector, and store the best as a SUGGESTION.
// Nothing is applied; `POST /api/memory/check/accept` is a person's act.
// The decision wire format answers yes/no questions and cannot write a
// pattern, so patterns are derived deterministically and the model judges.
func (s *Server) proposeChecks(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdmin(w, r) {
		return
	}
	st := s.adh()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "adherence store unavailable")
		return
	}
	client := s.decisionClient()
	if client == nil {
		writeErr(w, http.StatusServiceUnavailable, "no decision model configured (decision_url)")
		return
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 100)
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{Filter: filterFor(r, false), AcceptedOnly: true,
		Limit: 5000, Now: vault.Now()})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	have, _ := st.Checks()
	pending, _ := st.Suggestions()
	for _, g := range pending {
		have[g.Target] = adherence.Check{}
	}
	proposed, asked := 0, 0
	for _, hit := range hits {
		target := cues.FactTarget(hit.ID)
		if _, ok := have[target]; ok || hit.Untrusted() || !adherence.LooksLikeRule(hit.Category, hit.Text) {
			continue
		}
		cands := adherence.Candidates(hit.Text)
		if len(cands) == 0 {
			continue
		}
		var best adherence.Suggestion
		for _, pat := range cands {
			ctx, cancel := context.WithTimeout(r.Context(), 2*decide.DefaultTimeout)
			started := time.Now()
			state := "Rule:\n" + clipText(hit.Text, 800) + "\n\nRegular expression over the tool call:\n" + pat
			res, err := client.Ask(ctx, state, checkQuestion)
			cancel()
			s.bookDecision(client, res, started, err)
			asked++
			a, ok := res.Answers["detector"]
			if err != nil || !ok || a.Noul < proposeThreshold || a.Noul <= best.Confidence {
				continue
			}
			best = adherence.Suggestion{Check: adherence.Check{Target: target, Forbid: pat, Source: "proposed"},
				Confidence: a.Noul, Model: client.Model, Text: clipText(hit.Text, 200),
				Reason: fmt.Sprintf("model judged this pattern a faithful detector (p=%.2f)", a.Noul)}
		}
		if best.Target != "" {
			if st.PutSuggestion(best, time.Now()) == nil {
				proposed++
			}
		}
		if proposed >= limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposed": proposed, "model_calls": asked})
}
