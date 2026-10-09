package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/connectors"
)

// Connected sources as an agent extension.
//
// /api/sources* is what the MCP tools `sources`, `source_search`, `source_read`
// and `source_act` call; any HTTP client can use it too. The credentials never
// cross it. These are the OWNER's accounts, so on a multi-user instance only an
// administrator's key may use them. Deciding on an action is the owner's: approve and deny, the queue
// and the audit trail sit under /api/source-actions and /api/source-audit,
// which are administrative surface (see adminSurface) so that setting
// GRIMOIRE_ADMIN_TOKEN puts them out of an agent's reach. On an instance with
// no admin token the agent's own credentials could reach them — set one.

func (s *Server) sourceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/sources", s.adminOnly(s.listSources))
	mux.HandleFunc("POST /api/sources/{id}/search", s.adminOnly(s.searchSource))
	mux.HandleFunc("POST /api/sources/{id}/read", s.adminOnly(s.readSource))
	mux.HandleFunc("POST /api/sources/{id}/act", s.adminOnly(s.actOnSource))
	mux.HandleFunc("GET /api/sources/actions/{id}", s.adminOnly(s.sourceActionStatus))
	mux.HandleFunc("GET /api/source-actions", s.adminOnly(s.listSourceActions))
	mux.HandleFunc("POST /api/source-actions/{id}/approve", s.adminOnly(s.decideSourceAction(true)))
	mux.HandleFunc("POST /api/source-actions/{id}/deny", s.adminOnly(s.decideSourceAction(false)))
	mux.HandleFunc("GET /api/source-audit", s.adminOnly(s.sourceAudit))
}

// Sources returns the agent-facing service, or nil when connectors are off.
func (s *Server) Sources() *connectors.Service {
	if s.Connectors == nil {
		return nil
	}
	svc := &connectors.Service{Store: s.Connectors, Secrets: SecretsForConnectors{Server: s}}
	if s.Runner != nil {
		svc.Client = s.Runner.Client
	}
	return svc
}

func (s *Server) sourcesOr501(w http.ResponseWriter) *connectors.Service {
	svc := s.Sources()
	if svc == nil {
		writeErr(w, http.StatusNotImplemented, "connectors are unavailable")
	}
	return svc
}

// agentOf names the caller for the audit trail: a verified identity if there
// is one, else the self-asserted header.
func agentOf(r *http.Request) string {
	if n, ok := verifiedAgent(r); ok && n != "" {
		return n
	}
	if n := claimedAgent(r); n != "" {
		return n
	}
	return "agent"
}

func sourceErrStatus(err error) int {
	switch {
	case errors.Is(err, connectors.ErrNoSource), errors.Is(err, connectors.ErrNoAction):
		return http.StatusNotFound
	case errors.Is(err, connectors.ErrNotSupported), errors.Is(err, connectors.ErrBadParams):
		return http.StatusBadRequest
	case errors.Is(err, connectors.ErrActionDisabled):
		return http.StatusForbidden
	case errors.Is(err, connectors.ErrRateLimited):
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func (s *Server) listSources(w http.ResponseWriter, _ *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	list, err := svc.Sources()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) searchSource(w http.ResponseWriter, r *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	res, err := svc.Search(r.Context(), r.PathValue("id"), in.Query, agentOf(r), in.Limit)
	if err != nil {
		writeErr(w, sourceErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) readSource(w http.ResponseWriter, r *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	res, err := svc.Read(r.Context(), r.PathValue("id"), in.ID, agentOf(r))
	if err != nil {
		writeErr(w, sourceErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (s *Server) actOnSource(w http.ResponseWriter, r *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	var in struct {
		Action string         `json:"action"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<18)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	rec, err := svc.Act(r.Context(), r.PathValue("id"), in.Action, in.Params, agentOf(r))
	if err != nil {
		// A failed execution still returns the record, so the caller sees the
		// state and the reason together.
		if rec.ID != "" {
			writeJSON(w, http.StatusOK, agentView(rec))
			return
		}
		writeErr(w, sourceErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agentView(rec))
}

// agentView is the record as the requesting agent sees it: state and result,
// plus what happens next. The stored parameters are not echoed.
func agentView(rec connectors.ActionRecord) map[string]any {
	out := map[string]any{"id": rec.ID, "state": rec.State, "summary": rec.Summary}
	switch rec.State {
	case connectors.ActionPending:
		out["next"] = "waiting for the owner to approve (grimoire actions approve " + rec.ID +
			", or the web UI). Check with source_action_status; do not re-submit."
	case connectors.ActionExecuted:
		out["result"] = rec.Result
	case connectors.ActionFailed:
		out["error"] = rec.Error
	case connectors.ActionDenied:
		out["note"] = rec.Note
	}
	return out
}

func (s *Server) sourceActionStatus(w http.ResponseWriter, r *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	rec, err := svc.Status(r.PathValue("id"))
	if err != nil {
		writeErr(w, sourceErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, agentView(rec))
}

func (s *Server) listSourceActions(w http.ResponseWriter, r *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	list, err := svc.Actions(strings.TrimSpace(r.URL.Query().Get("state")), 200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *Server) decideSourceAction(approve bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		svc := s.sourcesOr501(w)
		if svc == nil {
			return
		}
		var in struct {
			Note string `json:"note"`
		}
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&in)
		by := "owner"
		if p := principal(r); p != nil && !p.Anonymous && p.User.Name != "" {
			by = p.User.Name
		}
		rec, err := svc.Decide(r.Context(), r.PathValue("id"), approve, by, in.Note)
		if err != nil && rec.ID == "" {
			writeErr(w, sourceErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, rec)
	}
}

func (s *Server) sourceAudit(w http.ResponseWriter, _ *http.Request) {
	svc := s.sourcesOr501(w)
	if svc == nil {
		return
	}
	list, err := svc.Audit(200)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, list)
}
