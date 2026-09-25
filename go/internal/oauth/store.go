package oauth

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, same as internal/db
)

// schema is this package's own tables, in its own database file.
//
// Not internal/db's Schema: that constant is cmd/grimoire's rebuildable
// index over the vault, opened by a process this one does not assume is even
// on the same host (GRIMOIRE_URL can point elsewhere). grimoire-mcp had no
// persistence of its own before this feature; these five tables are it,
// mirroring internal/db's conventions — WAL, a single writer connection, a
// busy timeout for the rare second writer, tokens stored hashed — without
// reopening or depending on the vault's database.
const schema = `
CREATE TABLE IF NOT EXISTS oauth_clients(
  id TEXT PRIMARY KEY,
  secret_hash TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  redirect_uris TEXT NOT NULL,
  auth_method TEXT NOT NULL DEFAULT 'none',
  created TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_codes(
  hash TEXT PRIMARY KEY,
  client_id TEXT NOT NULL,
  redirect_uri TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  resource TEXT NOT NULL DEFAULT '',
  code_challenge TEXT NOT NULL,
  code_challenge_method TEXT NOT NULL,
  expires REAL NOT NULL
);
CREATE TABLE IF NOT EXISTS oauth_tokens(
  hash TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  client_id TEXT NOT NULL,
  scope TEXT NOT NULL DEFAULT '',
  resource TEXT NOT NULL DEFAULT '',
  family TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL,
  expires REAL NOT NULL,
  last_used TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_client ON oauth_tokens(client_id);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_family ON oauth_tokens(family);
CREATE INDEX IF NOT EXISTS idx_oauth_tokens_kind ON oauth_tokens(kind);
`

// Client is an OAuth client registered via Dynamic Client Registration.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	AuthMethod   string // "none" (public/PKCE-only) or "client_secret_post"
	Created      time.Time
}

// hasSecret reports whether this client authenticates with a secret rather
// than PKCE alone.
func (c Client) hasSecret() bool { return c.AuthMethod != "" && c.AuthMethod != "none" }

// AuthCode is a single-use authorization code, consumed at the token
// endpoint.
type AuthCode struct {
	ClientID            string
	RedirectURI         string
	Scopes              []string
	Resource            string
	CodeChallenge       string
	CodeChallengeMethod string
}

// TokenPair is what the token endpoint hands back.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int64
	Scopes       []string
}

// TokenRecord is what a bearer lookup resolves to.
type TokenRecord struct {
	ClientID string
	Scopes   []string
	Resource string
	Family   string
}

var (
	// ErrNotFound covers an unknown client, an unknown or already-consumed
	// code, and an unknown, expired or revoked token — deliberately one
	// error for all three. Distinguishing "wrong" from "gone" at these
	// endpoints is exactly the enumeration oracle SECURITY.md's credential
	// requests section already avoids for the same reason.
	ErrNotFound = errors.New("oauth: not found")
	// ErrExpired reports a code or token past its own deadline. Kept
	// distinct from ErrNotFound only where the caller needs a different
	// message; callers that don't care can treat both as "reject".
	ErrExpired = errors.New("oauth: expired")
)

// Store is grimoire-mcp's OAuth state: registered clients, live
// authorization codes, and issued access/refresh tokens. One process, one
// file, one writer — same shape as internal/db, sized for the volume this
// actually sees (a handful of connectors, not a multi-tenant SaaS).
type Store struct {
	conn *sql.DB
	mu   sync.Mutex
}

// Open creates or opens the OAuth store at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	conn, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	conn.SetMaxOpenConns(1)
	if _, err := conn.Exec("PRAGMA journal_mode=WAL"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("enabling WAL: %w", err)
	}
	if _, err := conn.Exec("PRAGMA busy_timeout=5000"); err != nil {
		conn.Close()
		return nil, fmt.Errorf("setting busy timeout: %w", err)
	}
	if _, err := conn.Exec(schema); err != nil {
		conn.Close()
		return nil, fmt.Errorf("applying schema: %w", err)
	}
	return &Store{conn: conn}, nil
}

// Close releases the connection.
func (s *Store) Close() error { return s.conn.Close() }

func hashOf(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// CreateClient registers a client. secret is returned once, in plaintext —
// the store only ever holds its hash, the same rule sessions and API keys
// already follow.
func (s *Store) CreateClient(name string, redirectURIs []string, authMethod string) (*Client, string, error) {
	if len(redirectURIs) == 0 {
		return nil, "", errors.New("oauth: at least one redirect_uri is required")
	}
	id, err := randomToken()
	if err != nil {
		return nil, "", err
	}
	var secret, secretHash string
	if authMethod != "" && authMethod != "none" {
		authMethod = "client_secret_post" // the one confidential method this server issues
		secret, err = randomToken()
		if err != nil {
			return nil, "", err
		}
		secretHash = hashOf(secret)
	} else {
		authMethod = "none"
	}
	created := Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err = s.conn.Exec(
		"INSERT INTO oauth_clients(id, secret_hash, name, redirect_uris, auth_method, created) VALUES(?,?,?,?,?,?)",
		id, secretHash, name, strings.Join(redirectURIs, "\n"), authMethod, created.Format(time.RFC3339))
	if err != nil {
		return nil, "", err
	}
	return &Client{ID: id, Name: name, RedirectURIs: redirectURIs, AuthMethod: authMethod, Created: created}, secret, nil
}

// GetClient looks up a registered client by id.
func (s *Store) GetClient(id string) (*Client, error) {
	row := s.conn.QueryRow(
		"SELECT id, name, redirect_uris, auth_method, created FROM oauth_clients WHERE id=?", id)
	var c Client
	var uris, created string
	if err := row.Scan(&c.ID, &c.Name, &uris, &c.AuthMethod, &created); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	c.RedirectURIs = strings.Split(uris, "\n")
	c.Created, _ = time.Parse(time.RFC3339, created)
	return &c, nil
}

// CheckClientSecret verifies a client_secret in constant time. A public
// client (AuthMethod "none") has no secret to check and this always fails
// for one — callers must not call it for a public client.
func (s *Store) CheckClientSecret(clientID, presented string) bool {
	var hash string
	err := s.conn.QueryRow("SELECT secret_hash FROM oauth_clients WHERE id=?", clientID).Scan(&hash)
	if err != nil || hash == "" {
		return false
	}
	got := hashOf(presented)
	return subtle.ConstantTimeCompare([]byte(got), []byte(hash)) == 1
}

// SaveAuthCode stores a freshly issued authorization code, hashed, with a
// short TTL. The code itself is returned by the caller (server.go), never
// persisted in plaintext.
func (s *Store) SaveAuthCode(code string, ac AuthCode, ttl time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec(
		`INSERT INTO oauth_codes(hash, client_id, redirect_uri, scope, resource, code_challenge, code_challenge_method, expires)
		 VALUES(?,?,?,?,?,?,?,?)`,
		hashOf(code), ac.ClientID, ac.RedirectURI, joinScope(ac.Scopes), ac.Resource,
		ac.CodeChallenge, ac.CodeChallengeMethod, float64(Now().Add(ttl).Unix()))
	return err
}

// ConsumeAuthCode looks up and deletes a code atomically — a code redeemed
// twice is exactly the replay OAuth 2.1's authorization-code protection
// exists to prevent, so "read" and "invalidate" are one critical section
// under the store's write lock, the same shape internal/secrets uses for a
// limited-use grant.
func (s *Store) ConsumeAuthCode(code string) (*AuthCode, error) {
	h := hashOf(code)
	s.mu.Lock()
	defer s.mu.Unlock()
	row := s.conn.QueryRow(
		`SELECT client_id, redirect_uri, scope, resource, code_challenge, code_challenge_method, expires
		 FROM oauth_codes WHERE hash=?`, h)
	var ac AuthCode
	var scope string
	var expires float64
	if err := row.Scan(&ac.ClientID, &ac.RedirectURI, &scope, &ac.Resource,
		&ac.CodeChallenge, &ac.CodeChallengeMethod, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	// Deleted whether or not it turns out to be expired: a code is
	// single-use by definition, and leaving an expired one behind would
	// only let it be looked up again for no benefit.
	if _, err := s.conn.Exec("DELETE FROM oauth_codes WHERE hash=?", h); err != nil {
		return nil, err
	}
	if float64(Now().Unix()) > expires {
		return nil, ErrExpired
	}
	ac.Scopes = splitScope(scope)
	return &ac, nil
}

// pruneExpiredCodes drops codes nobody redeemed. Cheap enough to run
// opportunistically; there is no daemon here to schedule it otherwise.
func (s *Store) pruneExpiredCodes() {
	_, _ = s.conn.Exec("DELETE FROM oauth_codes WHERE expires < ?", float64(Now().Unix()))
}

const (
	accessTTL  = time.Hour
	refreshTTL = 30 * 24 * time.Hour
)

// IssueTokenPair mints an access token and a refresh token bound to the same
// client, scope and resource, sharing a family id so a later rotation can be
// traced back to the grant that started it.
func (s *Store) IssueTokenPair(clientID string, scopes []string, resource string) (*TokenPair, error) {
	family, err := randomToken()
	if err != nil {
		return nil, err
	}
	return s.issue(clientID, scopes, resource, family)
}

func (s *Store) issue(clientID string, scopes []string, resource, family string) (*TokenPair, error) {
	access, err := randomToken()
	if err != nil {
		return nil, err
	}
	refresh, err := randomToken()
	if err != nil {
		return nil, err
	}
	now := Now()
	scopeStr := joinScope(scopes)
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.conn.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck
	insert := `INSERT INTO oauth_tokens(hash, kind, client_id, scope, resource, family, created, expires)
	           VALUES(?,?,?,?,?,?,?,?)`
	if _, err := tx.Exec(insert, hashOf(access), "access", clientID, scopeStr, resource, family,
		now.Format(time.RFC3339), float64(now.Add(accessTTL).Unix())); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(insert, hashOf(refresh), "refresh", clientID, scopeStr, resource, family,
		now.Format(time.RFC3339), float64(now.Add(refreshTTL).Unix())); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: access, RefreshToken: refresh, ExpiresIn: int64(accessTTL.Seconds()), Scopes: scopes}, nil
}

// LookupAccessToken resolves a bearer token to what it may do, and records
// that it was used.
func (s *Store) LookupAccessToken(raw string) (*TokenRecord, error) {
	return s.lookup(raw, "access")
}

func (s *Store) lookup(raw, kind string) (*TokenRecord, error) {
	h := hashOf(raw)
	row := s.conn.QueryRow(
		"SELECT client_id, scope, resource, family, expires FROM oauth_tokens WHERE hash=? AND kind=?", h, kind)
	var rec TokenRecord
	var scope string
	var expires float64
	if err := row.Scan(&rec.ClientID, &scope, &rec.Resource, &rec.Family, &expires); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if float64(Now().Unix()) > expires {
		return nil, ErrExpired
	}
	rec.Scopes = splitScope(scope)
	// Best-effort — a lost "last used" timestamp costs nothing but a stale
	// admin-console column, never worth failing the request over.
	_, _ = s.conn.Exec("UPDATE oauth_tokens SET last_used=? WHERE hash=?", Now().Format(time.RFC3339), h)
	return &rec, nil
}

// RotateRefresh redeems a refresh token for a new token pair and revokes the
// one presented, so a stolen refresh token stops working the moment its
// legitimate owner's client next refreshes — the reuse-detection half of
// OAuth 2.1's refresh-token rotation requirement. It does not additionally
// revoke the rest of the family: distinguishing "the legitimate client rolled
// forward" from "an attacker just used a stolen token" needs tracking which
// token in a family is current, which the schema does not yet do, so this is
// simple rotation without that break-the-glass step. Narrowing scope on
// refresh is not offered — the client asks for what it was already granted.
func (s *Store) RotateRefresh(raw, clientID string) (*TokenPair, error) {
	rec, err := s.lookup(raw, "refresh")
	if err != nil {
		return nil, err
	}
	if rec.ClientID != clientID {
		// Same response as "no such token": a client_id that doesn't own the
		// presented refresh token learns nothing more than it would from a
		// typo, which is the point.
		return nil, ErrNotFound
	}
	s.mu.Lock()
	_, delErr := s.conn.Exec("DELETE FROM oauth_tokens WHERE hash=? AND kind='refresh'", hashOf(raw))
	s.mu.Unlock()
	if delErr != nil {
		return nil, delErr
	}
	return s.issue(rec.ClientID, rec.Scopes, rec.Resource, rec.Family)
}

// RevokeToken deletes a token, whichever kind it is. Per RFC 7009, an
// unknown token is not an error — the caller (server.go) always answers 200.
func (s *Store) RevokeToken(raw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec("DELETE FROM oauth_tokens WHERE hash=?", hashOf(raw))
	return err
}

// ClientSummary is one row of the admin console's client list.
type ClientSummary struct {
	ID           string
	Name         string
	RedirectURIs []string
	Created      time.Time
	LastUsed     time.Time
	ActiveTokens int
	Scopes       []string
}

// ListClients returns every registered client with its most recent token
// activity, newest first.
func (s *Store) ListClients() ([]ClientSummary, error) {
	rows, err := s.conn.Query("SELECT id, name, redirect_uris, created FROM oauth_clients ORDER BY created DESC")
	if err != nil {
		return nil, err
	}
	var out []ClientSummary
	for rows.Next() {
		var cs ClientSummary
		var uris, created string
		if err := rows.Scan(&cs.ID, &cs.Name, &uris, &created); err != nil {
			rows.Close()
			return nil, err
		}
		cs.RedirectURIs = strings.Split(uris, "\n")
		cs.Created, _ = time.Parse(time.RFC3339, created)
		out = append(out, cs)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	// Closed BEFORE tokenSummary runs its own query, not deferred: the pool
	// behind *sql.DB is a single connection (see Open), so a second query
	// issued while this one's rows are still open would block forever
	// waiting for a connection nothing was ever going to release.
	rows.Close()
	for i := range out {
		last, active, scopes, err := s.tokenSummary(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].LastUsed = last
		out[i].ActiveTokens = active
		out[i].Scopes = scopes
	}
	return out, nil
}

func (s *Store) tokenSummary(clientID string) (time.Time, int, []string, error) {
	rows, err := s.conn.Query(
		"SELECT scope, created, last_used, expires FROM oauth_tokens WHERE client_id=? AND kind='access'", clientID)
	if err != nil {
		return time.Time{}, 0, nil, err
	}
	defer rows.Close()
	var last time.Time
	var active int
	scopeSet := map[string]bool{}
	for rows.Next() {
		var scope, created, lastUsed string
		var expires float64
		if err := rows.Scan(&scope, &created, &lastUsed, &expires); err != nil {
			return time.Time{}, 0, nil, err
		}
		for _, sc := range splitScope(scope) {
			scopeSet[sc] = true
		}
		if float64(Now().Unix()) <= expires {
			active++
		}
		stamp := lastUsed
		if stamp == "" {
			stamp = created
		}
		if t, err := time.Parse(time.RFC3339, stamp); err == nil && t.After(last) {
			last = t
		}
	}
	scopes := make([]string, 0, len(scopeSet))
	for _, sc := range AllScopes {
		if scopeSet[sc] {
			scopes = append(scopes, sc)
		}
	}
	return last, active, scopes, rows.Err()
}

// RevokeClient deletes every token — access and refresh — issued to a
// client. This is the admin console's Revoke button: it does not delete the
// client registration, so the connector's next attempt lands back on the
// consent page rather than a registration error that just invites it to
// re-register under a new client_id.
func (s *Store) RevokeClient(clientID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.conn.Exec("DELETE FROM oauth_tokens WHERE client_id=?", clientID)
	return err
}
