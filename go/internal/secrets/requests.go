package secrets

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Asking for a grant that does not exist yet.
//
// Grants are scoped, time-boxed and revocable, which is the whole design — and
// until now they could only be minted AHEAD of the need, by a person, for a
// secret they guessed an agent would want. An agent that hits a secret it has
// no grant for just fails. That sounds safe and is not, because it selects for
// the only workable habit: pre-granting broadly, with long TTLs, for everything
// an agent MIGHT need. A vault whose grants are all "any scope, 24 hours,
// issued last Tuesday" has the ceremony of least privilege and none of it.
//
// So an agent can ask. The request records what it wants and WHY, in its own
// words, and a person answers — approve once, approve with a TTL, or deny.
// Nothing is issued by asking: a pending request confers no access at all, and
// the approval step is where the existing Grant path runs, unchanged.
//
// Three properties are deliberate:
//
//   - Asking requires no unlocked vault. The vault is usually locked precisely
//     when nobody is at the keyboard, which is when an agent needs to leave a
//     request rather than get an error it cannot act on. Approving requires
//     one, because approving mints a grant.
//   - A request names a secret that may not exist. Refusing unknown names here
//     would turn this route into an oracle for which secrets a vault holds,
//     answerable by anyone who can ask. The name is validated at APPROVAL,
//     where a person is already looking at it.
//   - The token is returned exactly once, to the agent that asked, after
//     approval. It is stored so the approving human never has to hand a
//     credential token around out of band, and it is what makes "approve on a
//     phone, agent continues" work.

// RequestState is where a request has got to.
const (
	StatePending  = "pending"
	StateApproved = "approved"
	StateDenied   = "denied"
)

// Request is one agent's ask.
type Request struct {
	ID      string `json:"id"`
	Secret  string `json:"secret"`
	Grantee string `json:"grantee"`
	Scope   string `json:"scope"`
	Reason  string `json:"reason"`
	TTL     int    `json:"ttl_seconds"`
	State   string `json:"state"`
	Created string `json:"created"`
	Decided string `json:"decided,omitempty"`
	// DecidedBy is the account that answered, for the audit trail. Empty on a
	// single-user instance, where there is only one person it could be.
	DecidedBy string `json:"decided_by,omitempty"`
	// Note is the human's reason for a denial, which is the half an agent can
	// actually act on: "use the read-only key instead" ends a loop that "no"
	// would restart.
	Note string `json:"note,omitempty"`
	// Token is the issued grant, and is ONLY ever populated for the agent that
	// asked, reading its own approved request. Listing never fills it in.
	Token string `json:"token,omitempty"`
}

// Limits on what a request may say. These are not security boundaries — the
// request is inert — they stop a runaway agent from filling the table with
// megabytes of "reason".
const (
	maxReasonLen  = 500
	maxRequestTTL = 24 * 3600
)

// ErrNoRequest is returned for an id that is not a pending request.
var ErrNoRequest = errors.New("unknown or already-decided request")

func requestID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RequestGrant records an agent's ask. It issues nothing.
func (b *Broker) RequestGrant(secretName, grantee, scope, reason string, ttlSeconds int) (Request, error) {
	secretName = strings.TrimSpace(secretName)
	grantee = strings.TrimSpace(grantee)
	if secretName == "" {
		return Request{}, errors.New("secret required")
	}
	if grantee == "" {
		return Request{}, errors.New("grantee required")
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 900
	}
	if ttlSeconds > maxRequestTTL {
		ttlSeconds = maxRequestTTL
	}
	reason = strings.TrimSpace(reason)
	if len(reason) > maxReasonLen {
		reason = reason[:maxReasonLen]
	}

	// One pending request per (secret, grantee, scope). An agent in a retry
	// loop would otherwise post a hundred identical asks and bury the one a
	// person was about to approve — and approving any of them is the same act.
	var existingID string
	err := b.DB.QueryRow(
		"SELECT id FROM grant_requests WHERE state=? AND secret=? AND grantee=? AND scope=?",
		StatePending, secretName, grantee, scope).Scan(&existingID)
	if err == nil {
		return b.Request(existingID, grantee)
	}

	id, err := requestID()
	if err != nil {
		return Request{}, err
	}
	now := Now().Format(time.RFC3339)
	if err := b.DB.Exec(
		"INSERT INTO grant_requests(id,secret,grantee,scope,reason,ttl,state,created)"+
			" VALUES(?,?,?,?,?,?,?,?)",
		id, secretName, grantee, scope, reason, ttlSeconds, StatePending, now); err != nil {
		return Request{}, err
	}
	// Audited like every other thing that happens to a secret. An ask that
	// was never approved is still worth being able to see afterwards.
	b.audit("grant_requested", secretName,
		fmt.Sprintf("grantee=%s scope=%s ttl=%ds reason=%s", grantee, scope, ttlSeconds, reason))
	return Request{ID: id, Secret: secretName, Grantee: grantee, Scope: scope,
		Reason: reason, TTL: ttlSeconds, State: StatePending, Created: now}, nil
}

// Requests lists asks, newest first. state="" lists every state.
//
// Never returns a token: this is the human's view, and a listing that carried
// live credentials would make the console's own polling the widest disclosure
// surface in the product.
func (b *Broker) Requests(state string, limit int) ([]Request, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := "SELECT id,secret,grantee,scope,reason,ttl,state,created," +
		"COALESCE(decided,''),COALESCE(decided_by,''),COALESCE(note,'') FROM grant_requests"
	args := []any{}
	if state != "" {
		q += " WHERE state=?"
		args = append(args, state)
	}
	q += " ORDER BY created DESC, id LIMIT ?"
	args = append(args, limit)

	rows, err := b.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Request{}
	for rows.Next() {
		var r Request
		if err := rows.Scan(&r.ID, &r.Secret, &r.Grantee, &r.Scope, &r.Reason,
			&r.TTL, &r.State, &r.Created, &r.Decided, &r.DecidedBy, &r.Note); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Request returns one ask to the agent that made it, with the token if it was
// approved.
//
// grantee is checked rather than trusted: this is the ONE read that can return
// a live credential token, so it answers only the caller it was issued for. An
// empty grantee is a caller that did not say who it is, and gets nothing.
func (b *Broker) Request(id, grantee string) (Request, error) {
	if strings.TrimSpace(grantee) == "" {
		return Request{}, ErrNoRequest
	}
	var r Request
	var token sql.NullString
	err := b.DB.QueryRow(
		"SELECT id,secret,grantee,scope,reason,ttl,state,created,"+
			"COALESCE(decided,''),COALESCE(decided_by,''),COALESCE(note,''),token"+
			" FROM grant_requests WHERE id=? AND grantee=?", id, grantee).
		Scan(&r.ID, &r.Secret, &r.Grantee, &r.Scope, &r.Reason, &r.TTL, &r.State,
			&r.Created, &r.Decided, &r.DecidedBy, &r.Note, &token)
	if err != nil {
		return Request{}, ErrNoRequest
	}
	r.Token = token.String
	return r, nil
}

// Approve mints the grant a request asked for and records the decision.
//
// ttlOverride lets the person shorten (or lengthen) what was asked for, which
// is most of the point of a human in the loop: an agent that asks for a day
// can be given ten minutes without anyone having to explain why.
func (b *Broker) Approve(id, decidedBy string, ttlOverride int) (Request, error) {
	if !b.Vault.IsUnlocked() {
		return Request{}, ErrLocked
	}
	r, err := b.pending(id)
	if err != nil {
		return Request{}, err
	}
	ttl := r.TTL
	if ttlOverride > 0 {
		ttl = ttlOverride
	}
	// The existing Grant path, unchanged — including its check that the secret
	// exists. Approving a request for a secret nobody ever stored fails HERE,
	// in front of the person who can fix it, rather than at asking time in
	// front of an agent that would learn the vault's contents from the error.
	token, err := b.Grant(r.Secret, r.Grantee, r.Scope, ttl)
	if err != nil {
		return Request{}, err
	}
	now := Now().Format(time.RFC3339)
	if err := b.DB.Exec(
		"UPDATE grant_requests SET state=?, decided=?, decided_by=?, token=?, ttl=? WHERE id=?",
		StateApproved, now, decidedBy, token, ttl, id); err != nil {
		return Request{}, err
	}
	b.audit("grant_approved", r.Secret,
		fmt.Sprintf("request=%s grantee=%s ttl=%ds by=%s", id, r.Grantee, ttl, decidedBy))
	r.State, r.Decided, r.DecidedBy, r.Token, r.TTL = StateApproved, now, decidedBy, token, ttl
	return r, nil
}

// Deny records a refusal, with an optional note the agent can act on.
//
// It does NOT require an unlocked vault: saying no mints nothing, and needing
// to unlock the credential store in order to refuse access to it is backwards.
func (b *Broker) Deny(id, decidedBy, note string) (Request, error) {
	r, err := b.pending(id)
	if err != nil {
		return Request{}, err
	}
	note = strings.TrimSpace(note)
	if len(note) > maxReasonLen {
		note = note[:maxReasonLen]
	}
	now := Now().Format(time.RFC3339)
	if err := b.DB.Exec(
		"UPDATE grant_requests SET state=?, decided=?, decided_by=?, note=? WHERE id=?",
		StateDenied, now, decidedBy, note, id); err != nil {
		return Request{}, err
	}
	b.audit("grant_denied", r.Secret,
		fmt.Sprintf("request=%s grantee=%s by=%s note=%s", id, r.Grantee, decidedBy, note))
	r.State, r.Decided, r.DecidedBy, r.Note = StateDenied, now, decidedBy, note
	return r, nil
}

// pending loads a request that has not been decided yet. Deciding an already
// decided request must fail rather than mint a second grant: an approval that
// can be replayed is an approval that can be replayed by anyone who kept the id.
func (b *Broker) pending(id string) (Request, error) {
	var r Request
	err := b.DB.QueryRow(
		"SELECT id,secret,grantee,scope,reason,ttl,state,created FROM grant_requests"+
			" WHERE id=? AND state=?", id, StatePending).
		Scan(&r.ID, &r.Secret, &r.Grantee, &r.Scope, &r.Reason, &r.TTL, &r.State, &r.Created)
	if err != nil {
		return Request{}, ErrNoRequest
	}
	return r, nil
}

// PendingCount is what a badge shows. Cheap enough to poll.
func (b *Broker) PendingCount() (int, error) {
	return b.DB.Count("SELECT COUNT(*) FROM grant_requests WHERE state=?", StatePending)
}
