package api

import (
	"errors"
	"net/http"

	"github.com/JeremiahM37/grimoire/go/internal/calibrate"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// Calibration hooks. Each write path that already knows a fact's outcome calls
// one of these after its own write has succeeded. They are best effort by
// design: a failed counter must never fail the write that caused the event,
// because the counter is telemetry about the write, not part of it.

// calibrateEvent attributes one correction signal to the agent that wrote the
// fact at note/id. Human-authored and hand-edited facts have no agent to score,
// and an unknown fact is ignored.
func (s *Server) calibrateEvent(note, id string, ev calibrate.Event) {
	if s.Index == nil || s.Vault == nil || id == "" {
		return
	}
	n, err := s.Vault.Read(note)
	if err != nil {
		return
	}
	e, ok := entryInBody(n.Body, id)
	if !ok || e.Agent == "" || e.Human || e.HandWritten {
		return
	}
	st, err := calibrate.Open(s.Index.DB)
	if err != nil {
		return
	}
	_ = st.Bump(e.Agent, ev)
}

var errNoIndex = errors.New("memory index is not available")

// calibrationStore opens the counter tables. Nil when the index is absent.
func (s *Server) calibrationStore() (*calibrate.Store, error) {
	if s.Index == nil {
		return nil, errNoIndex
	}
	return calibrate.Open(s.Index.DB)
}

// noteRecalled counts one recall of every fact a recall returned. It is the
// only calibration write on the read path, and it is best effort.
func (s *Server) noteRecalled(hits []index.MemoryHit) {
	if len(hits) == 0 {
		return
	}
	st, err := s.calibrationStore()
	if err != nil {
		return
	}
	for _, h := range hits {
		_ = st.NoteRecalled(h.Note, h.ID)
	}
}

// attachReliability adds the writer's conservative reliability to every
// agent-written fact in an explain response. Human and unattributed facts get
// nothing, because there is no agent to score.
func (s *Server) attachReliability(out []entryOut) {
	st, err := s.calibrationStore()
	if err != nil {
		return
	}
	cache := map[string]float64{}
	for i := range out {
		agent := out[i].Agent
		if agent == "" {
			continue
		}
		v, ok := cache[agent]
		if !ok {
			r, err := st.Reliability(agent)
			if err != nil {
				continue
			}
			v = r
			cache[agent] = v
		}
		out[i].AgentReliability = &v
	}
}

// GET /api/memory/agents/reliability
//
// Admin only. One row per agent with at least one correction signal, lowest
// reliability first. Counts are correction events, not facts, and the
// reliability is a conservative lower bound (see the calibrate package), so a
// new agent with no record shows the prior and not zero.
func (s *Server) agentReliability(w http.ResponseWriter, r *http.Request) {
	st, err := s.calibrationStore()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	rows, err := st.Agents()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rows == nil {
		rows = []calibrate.AgentRow{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"agents": rows,
		"note": "counts are correction events (confirmed, challenged, superseded by a person, deleted by a person). " +
			"reliability is a 95% lower bound on the share of an agent's corrected facts that stood; " +
			"it measures how often an agent was corrected, not whether its facts are true.",
	})
}
