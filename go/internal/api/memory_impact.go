package api

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/impact"
)

// GET /api/memory/impact: friction in agent sessions before and after each
// memory and each exported skill landed. Correlation only, and the reply says
// so (see internal/impact). It reads the session transcripts on this host, so
// it is for administrators only, like the memory core's canonical-store view.
//
//	since   how far back to read sessions: 30d, 90d (default), 12h
//	agent   only this agent's sessions
//	min     sessions needed on each side of a date (default 5)
//	limit   rows returned (default 60)
//	retells=1  instead: the weekly re-tell series over the whole transcript
//	           history (or since=), judged by the configured re-tell judge or a
//	           strict lexical rule; cutoff= (default 2026-10-09, when injection
//	           went live) splits BEFORE from AFTER; budget= caps new judge calls
//	           (default 2000, beyond which a fixed sample is judged). Judged
//	           pairs are cached in .grimoire/retell-judged.jsonl. Only sessions a
//	           person drove count (no headless runs, benchmark harnesses, Lectern
//	           workers, machine-written prompts); all=1 turns that off.
//
// Reading and parsing transcripts is the slow part, so a result is kept for
// two minutes.

var impactCache = struct {
	sync.Mutex
	key    string
	at     time.Time
	report impact.Report
	errs   []string
}{}

// ParseSince reads "30d", "12h" or "2w".
func ParseSince(v string) (time.Duration, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0, nil
	}
	unit := v[len(v)-1]
	n, err := strconv.Atoi(v[:len(v)-1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("since %q: want a number and d, h or w (30d)", v)
	}
	switch unit {
	case 'd':
		return time.Duration(n) * 24 * time.Hour, nil
	case 'w':
		return time.Duration(n) * 7 * 24 * time.Hour, nil
	case 'h':
		return time.Duration(n) * time.Hour, nil
	}
	return 0, fmt.Errorf("since %q: want a number and d, h or w (30d)", v)
}

func (s *Server) memoryImpact(w http.ResponseWriter, r *http.Request) {
	if p := principal(r); !(p.Unrestricted || p.IsAdmin()) {
		writeErr(w, http.StatusForbidden, "session impact reads transcripts on this host; administrators only")
		return
	}
	q := r.URL.Query()
	since, err := ParseSince(q.Get("since"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if q.Get("retells") == "1" {
		s.memoryRetells(w, r, since)
		return
	}
	atoi := func(k string) int { n, _ := strconv.Atoi(q.Get(k)); return max(n, 0) }
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	opt := impact.CollectOptions{Home: home, Store: s.canonicalMemoryDir(), Since: since, Agent: q.Get("agent"),
		Min: atoi("min"), Limit: atoi("limit")}
	key := fmt.Sprintf("%s|%s|%v|%s|%d|%d", home, opt.Store, since, opt.Agent, opt.Min, opt.Limit)

	impactCache.Lock()
	defer impactCache.Unlock()
	if impactCache.key != key || time.Since(impactCache.at) > 2*time.Minute {
		rep, errs := impact.Collect(opt)
		impactCache.key, impactCache.at, impactCache.report = key, time.Now(), rep
		impactCache.errs = impactCache.errs[:0]
		for i, e := range errs {
			if i == 10 {
				break
			}
			impactCache.errs = append(impactCache.errs, e.Error())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"report": impactCache.report, "errors": impactCache.errs})
}

// DefaultRetellCutoff is when the memory injection hook went live.
const DefaultRetellCutoff = "2026-10-09"

var retellCache = struct {
	sync.Mutex
	key    string
	at     time.Time
	report impact.RetellReport
	errs   []string
}{}

func (s *Server) memoryRetells(w http.ResponseWriter, r *http.Request, since time.Duration) {
	q := r.URL.Query()
	cutoffText := q.Get("cutoff")
	if cutoffText == "" {
		cutoffText = DefaultRetellCutoff
	}
	cutoff, err := time.Parse("2006-01-02", cutoffText)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "cutoff must be YYYY-MM-DD")
		return
	}
	budget, _ := strconv.Atoi(q.Get("budget"))
	home, err := os.UserHomeDir()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	co := impact.CollectOptions{Home: home, Store: s.canonicalMemoryDir(), Since: since, Agent: q.Get("agent"), Now: time.Now(), AllSessions: q.Get("all") == "1"}
	ro := impact.RetellOptions{Cutoff: cutoff, Budget: budget, CachePath: filepath.Join(s.Vault.Root, ".grimoire", "retell-judged.jsonl")}
	if c := s.retellClient(); c != nil {
		threshold := 0.9
		if v, err := strconv.ParseFloat(s.setting("retell_threshold"), 64); err == nil && v > 0 && v < 1 {
			threshold = v
		}
		ro.JudgeName = "decision-model:" + c.Model + "@" + strconv.FormatFloat(threshold, 'f', 2, 64)
		ro.Judge = func(memory, prompt string) (bool, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*decide.DefaultTimeout)
			defer cancel()
			res, err := c.Ask(ctx, "Stored memory:\n"+memory+"\n\nUser's message to the agent:\n"+prompt, retellQuestion)
			if err != nil {
				return false, err
			}
			a, ok := res.Answers["retell"]
			if !ok || a.Type != decide.Noul {
				return false, fmt.Errorf("no retell answer")
			}
			return a.Noul >= threshold, nil
		}
	}
	key := fmt.Sprintf("%s|%s|%v|%s|%s|%d|%s|%v", home, co.Store, since, co.Agent, cutoffText, budget, ro.JudgeName, co.AllSessions)
	retellCache.Lock()
	defer retellCache.Unlock()
	if retellCache.key != key || time.Since(retellCache.at) > 30*time.Minute {
		rep, errs := impact.RetellCollect(co, ro)
		retellCache.key, retellCache.at, retellCache.report = key, time.Now(), rep
		retellCache.errs = nil
		for i, e := range errs {
			if i == 10 {
				break
			}
			retellCache.errs = append(retellCache.errs, e.Error())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"retells": retellCache.report, "errors": retellCache.errs})
}
