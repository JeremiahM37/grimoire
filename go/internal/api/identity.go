package api

import (
	"net/http"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/identity"
)

// Network identity, wired into the two things that were asking different
// questions and getting the same unverified answer.
//
// The split is deliberate and it is the whole design:
//
//   - ATTRIBUTION always uses it. A verified caller's name replaces the
//     self-asserted header everywhere a name is recorded, so the ledger, the
//     read-audit trail and the authority lattice stop taking the caller's word
//     for who it is.
//
//   - AUTHORIZATION uses it only through an explicit mapping the operator
//     created. Knowing truthfully who is calling says nothing about what they
//     may read; somebody has to decide that once, on purpose. An unmapped
//     identity is attributed and otherwise treated exactly as before, which is
//     also what keeps single-user deployments unaffected — with no accounts
//     there is nothing to map and nothing changes.

// identityKey carries the resolved identity through the request.
type identityKey struct{}

// identityOf returns the network identity the middleware resolved, if any.
func identityOf(r *http.Request) (identity.Identity, bool) {
	if r == nil {
		return identity.Identity{}, false
	}
	id, ok := r.Context().Value(identityKey{}).(identity.Identity)
	return id, ok
}

// verifiedAgent is the caller's name when something other than the caller
// established it.
//
// Device before user: the ledger and the audit trail answer "which machine did
// this", and one person's four machines are four callers. The user is carried
// alongside rather than instead.
func verifiedAgent(r *http.Request) (string, bool) {
	id, ok := identityOf(r)
	if !ok || !id.Verified {
		return "", false
	}
	if id.Device != "" {
		return id.Device, true
	}
	if id.User != "" {
		return id.User, true
	}
	return strings.TrimPrefix(id.Subject, id.Backend+":"), id.Subject != ""
}

// whoami reports what the server makes of the caller.
//
// Exposed because the failure mode of identity configuration is silence: a
// backend that never matches looks exactly like one that is working, right up
// until an audit trail turns out to be full of self-asserted names. This says
// which mechanisms are running, which one answered, and — the part that makes
// it useful — what the caller CLAIMED alongside what was verified, so the two
// can be compared.
func (s *Server) identityWhoami(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"backends": s.Identity.Names(),
		"enabled":  s.Identity.Enabled(),
		"claimed":  claimedAgent(r),
		"verified": false,
		"peer":     "",
		"scope":    "Attribution always uses a verified identity. Authorization uses it only where you have mapped it to an account.",
	}
	if peer, ok := identity.PeerAddr(r); ok {
		out["peer"] = peer.String()
	}
	if id, ok := identityOf(r); ok {
		out["verified"] = id.Verified
		out["identity"] = id
	}
	// The name that will actually be recorded, which is the question an
	// operator checking their configuration is really asking.
	out["attributed_to"] = agentFor(r)
	writeJSON(w, http.StatusOK, out)
}

// identitySubject is the external id used in the account mapping.
//
// The backend-qualified subject is stripped of its prefix, because the mapping
// table already records the source in its own column and "tailscale" /
// "tailscale:jam@github" would store the word twice and match neither way
// round.
func identitySubject(id identity.Identity) string {
	return strings.TrimPrefix(id.Subject, id.Backend+":")
}
