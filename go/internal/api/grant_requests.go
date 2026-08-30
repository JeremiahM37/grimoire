package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// The just-in-time half of the credential broker: an agent asks, a person
// answers. See internal/secrets/requests.go for why this exists at all.
//
// The route classes here are the interesting part, and they are not uniform:
//
//	POST   /api/secrets/requests        authed — an agent asks
//	GET    /api/secrets/requests/{id}   authed — the asker collects its answer
//	GET    /api/secrets/requests        admin  — the human's queue
//	POST   .../{id}/approve             admin  — mints a grant
//	POST   .../{id}/deny                admin
//
// Asking cannot be admin-gated, or no agent could ever ask. Answering cannot
// be anything BUT admin-gated, because answering hands out a credential. The
// asymmetry is the feature.

type grantRequestIn struct {
	Secret     string `json:"secret"`
	Grantee    string `json:"grantee"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
	TTLSeconds int    `json:"ttl_seconds"`
	// MaxUses lets the asking agent bound its own request. The agent knows
	// what it is about to do; the approver is guessing.
	MaxUses int `json:"max_uses"`
}

func (s *Server) requestGrant(w http.ResponseWriter, r *http.Request) {
	var in grantRequestIn
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req, err := s.Broker.RequestGrant(in.Secret, in.Grantee, in.Scope, in.Reason,
		in.TTLSeconds, in.MaxUses)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// 202, not 201: the server has ACCEPTED the ask and done nothing else.
	// Answering 200 with a pending request is how a caller ends up believing
	// it has access it does not have.
	writeJSON(w, http.StatusAccepted, req)
}

// pollGrantRequest is how an agent collects its answer — and the only read in
// the product that can return a live grant token, so it answers to the grantee
// it was issued for and nobody else.
func (s *Server) pollGrantRequest(w http.ResponseWriter, r *http.Request) {
	grantee := strings.TrimSpace(r.URL.Query().Get("grantee"))
	req, err := s.Broker.Request(r.PathValue("id"), grantee)
	if err != nil {
		// One error for "no such request" and for "not yours". Distinguishing
		// them would turn this into an oracle for which request ids exist.
		writeErr(w, http.StatusNotFound, "unknown request")
		return
	}
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) listGrantRequests(w http.ResponseWriter, r *http.Request) {
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.Broker.Requests(state, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	pending, _ := s.Broker.PendingCount()
	writeJSON(w, http.StatusOK, map[string]any{"requests": out, "pending": pending})
}

func (s *Server) approveGrantRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTLSeconds int `json:"ttl_seconds"`
	}
	// A body is optional here: approving as asked is the common case, and
	// requiring a JSON object to say "yes, as requested" makes the one-tap
	// approval this feature exists for impossible.
	_ = json.NewDecoder(r.Body).Decode(&in)

	req, err := s.Broker.Approve(r.PathValue("id"), whoDecided(r), in.TTLSeconds)
	if s.vaultLocked(w, err) {
		return
	}
	if errors.Is(err, secrets.ErrNoRequest) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// The token is deliberately NOT echoed to the approver. They did not ask
	// for a credential, they answered a question about one, and a token on
	// screen is a token in a screenshot.
	req.Token = ""
	writeJSON(w, http.StatusOK, req)
}

func (s *Server) denyGrantRequest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)

	req, err := s.Broker.Deny(r.PathValue("id"), whoDecided(r), in.Note)
	if errors.Is(err, secrets.ErrNoRequest) {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// whoDecided names the account that answered, for the trail. On a single-user
// instance there is no account, and "" is the honest answer rather than a
// fabricated one.
func whoDecided(r *http.Request) string {
	p := principal(r)
	if p == nil || p.Anonymous || p.Unrestricted {
		return ""
	}
	return p.User.Name
}
