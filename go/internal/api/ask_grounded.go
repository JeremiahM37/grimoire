package api

// The grounded answer mode of /api/ask (and of the ask_notes MCP tool).
//
// Plain ask hands the retrieved passages to one model call. Grounded ask runs
// the gather / answer / self-check procedure in internal/grounded over the
// same passages -- the whole vault when it fits the context budget, ranked
// passages otherwise -- with relative times ("last Saturday") resolved against
// each note's own date, and, when a memory bank is named, that bank's entity
// timeline. It costs two model calls instead of one, so it is opt-in.

import (
	"context"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/grounded"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

var pathDate = regexp.MustCompile(`(\d{4})-(\d{2})-(\d{2})`)

// noteDate is the date a note's relative times are relative to: a date in its
// filename (daily notes), else its created stamp, else its modification time.
func (s *Server) noteDate(path string) time.Time {
	if m := pathDate.FindStringSubmatch(path); m != nil {
		if t, err := time.Parse("2006-01-02", m[0]); err == nil {
			return t
		}
	}
	var created string
	var mtime float64
	if err := s.Index.DB.QueryRow("SELECT COALESCE(created,''), COALESCE(mtime,0) FROM notes WHERE path=?", path).Scan(&created, &mtime); err != nil {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.Parse(layout, strings.TrimSpace(created)); err == nil {
			return t
		}
	}
	if mtime > 0 {
		return time.Unix(int64(mtime), 0).UTC()
	}
	return time.Time{}
}

// groundedAnswer answers q from hits with the three-step procedure.
func (s *Server) groundedAnswer(w http.ResponseWriter, r *http.Request, q string, hits []index.Hit, mode, bankID string) {
	ps := make([]grounded.Passage, 0, len(hits))
	for _, h := range hits {
		ps = append(ps, grounded.Passage{Title: h.Title, Date: s.noteDate(h.Path), Text: h.Chunk})
	}
	src := grounded.RetrievalSource{
		Annotate: true,
		Fetch:    func(_ context.Context, _ string) ([]grounded.Passage, error) { return ps, nil },
	}
	opt := grounded.Options{Procedure: true}
	if bankID != "" && s.Banks != nil {
		facts, _, err := s.Banks.ListFacts(bankID, bank.FactQuery{Limit: 5000})
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bank: "+err.Error())
			return
		}
		src.Timeline = grounded.TimelineFromFacts(facts)
		opt.Timeline = true
	}
	ans := &grounded.Answerer{LLM: grounded.ClientLLM{C: s.AI.WithSurface("ask_grounded", agentFor(r))}, Opt: opt}
	res, err := ans.Answer(r.Context(), q, src)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "grounded answer: "+err.Error())
		return
	}
	cites := []map[string]any{}
	for _, h := range hits {
		cites = append(cites, map[string]any{"path": h.Path, "title": h.Title, "trust": h.Trust})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"answer": res.Answer, "citations": cites, "mode": mode, "grounded": true,
		"evidence": res.Evidence, "check": res.Check, "calls": res.Calls,
		"timeline_events": res.Events})
}
