package secrets

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// Grants are the USE-not-READ mechanism: a scoped, expiring token that lets a
// holder make ONE KIND of authenticated request without ever receiving the
// credential. The scope is a URL prefix, so a grant for one API cannot be
// redirected at another, and expiry bounds the blast radius of a leaked token.

// Grant is what list_grants returns — deliberately without any value.
type Grant struct {
	Token     string  `json:"token"`
	Secret    string  `json:"secret"`
	Grantee   string  `json:"grantee"`
	Scope     string  `json:"scope"`
	ExpiresAt float64 `json:"expires_at"`
	Created   string  `json:"created"`
}

// Broker performs outbound requests with a secret injected.
type Broker struct {
	Vault  *Vault
	DB     *db.DB
	Client *http.Client
}

func NewBroker(v *Vault, database *db.DB) *Broker {
	return &Broker{Vault: v, DB: database, Client: &http.Client{Timeout: 60 * time.Second}}
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Grant issues a scoped token for a secret. It requires an unlocked vault: only
// the human, present, may hand out access.
func (b *Broker) Grant(secretName, grantee, scope string, ttlSeconds int) (string, error) {
	if !b.Vault.IsUnlocked() {
		return "", ErrLocked
	}
	if _, err := b.Vault.Get(secretName); err != nil {
		return "", err
	}
	if strings.TrimSpace(grantee) == "" {
		return "", errors.New("grantee required")
	}
	if ttlSeconds <= 0 {
		ttlSeconds = 900
	}
	token, err := newToken()
	if err != nil {
		return "", err
	}
	expires := float64(Now().Add(time.Duration(ttlSeconds) * time.Second).Unix())
	if err := b.DB.Exec(
		"INSERT INTO grants(token,secret,grantee,scope,expires_at,created) VALUES(?,?,?,?,?,?)",
		token, secretName, grantee, scope, expires, Now().Format(time.RFC3339)); err != nil {
		return "", err
	}
	b.audit("grant", secretName, fmt.Sprintf("grantee=%s scope=%s ttl=%ds", grantee, scope, ttlSeconds))
	return token, nil
}

// List returns active grants, never values.
func (b *Broker) List() ([]Grant, error) {
	if !b.Vault.IsUnlocked() {
		return nil, ErrLocked
	}
	rows, err := b.DB.Query(
		"SELECT token, secret, grantee, scope, expires_at, created FROM grants " +
			"ORDER BY created DESC, token")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Grant{}
	for rows.Next() {
		var g Grant
		var created sql.NullString
		if err := rows.Scan(&g.Token, &g.Secret, &g.Grantee, &g.Scope, &g.ExpiresAt, &created); err != nil {
			return nil, err
		}
		g.Created = created.String
		out = append(out, g)
	}
	return out, nil
}

// Revoke drops one grant.
func (b *Broker) Revoke(token string) error {
	if !b.Vault.IsUnlocked() {
		return ErrLocked
	}
	b.audit("revoke", "", "token="+shortToken(token))
	return b.DB.Exec("DELETE FROM grants WHERE token=?", token)
}

// RevokeAll drops every grant — the panic button.
func (b *Broker) RevokeAll() error {
	if !b.Vault.IsUnlocked() {
		return ErrLocked
	}
	b.audit("revoke_all", "", "")
	return b.DB.Exec("DELETE FROM grants")
}

// Use makes the request with the secret injected into the chosen header. The
// caller receives the RESPONSE only; the credential never leaves this process.
func (b *Broker) Use(token, method, targetURL, header, body string) (map[string]any, error) {
	if !b.Vault.IsUnlocked() {
		return nil, ErrLocked
	}
	var g Grant
	err := b.DB.QueryRow(
		"SELECT token, secret, grantee, scope, expires_at FROM grants WHERE token=?", token,
	).Scan(&g.Token, &g.Secret, &g.Grantee, &g.Scope, &g.ExpiresAt)
	if err != nil {
		return nil, errors.New("unknown or revoked grant")
	}
	if float64(Now().Unix()) > g.ExpiresAt {
		// clean up as we go: an expired grant should stop appearing in the console
		_ = b.DB.Exec("DELETE FROM grants WHERE token=?", token)
		return nil, errors.New("grant expired")
	}
	if g.Scope != "" && !strings.HasPrefix(targetURL, g.Scope) {
		// the scope is what stops a token for one API being pointed at another
		b.audit("denied", g.Secret, "url="+targetURL+" scope="+g.Scope)
		return nil, fmt.Errorf("url outside grant scope %q", g.Scope)
	}
	value, err := b.Vault.Get(g.Secret)
	if err != nil {
		return nil, err
	}
	if method == "" {
		method = "GET"
	}
	if header == "" {
		header = "Authorization"
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(strings.ToUpper(method), targetURL, rdr)
	if err != nil {
		return nil, err
	}
	// injected verbatim, NOT Bearer-prefixed: an X-Api-Key style header would
	// break if a scheme were assumed. Callers encode any scheme in the secret.
	req.Header.Set(header, value)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := b.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	b.audit("broker", g.Secret, fmt.Sprintf("%s %s -> %d", method, targetURL, resp.StatusCode))
	return map[string]any{
		"status": resp.StatusCode,
		"body":   string(raw),
	}, nil
}

// Record writes an audit entry from outside this package (secret set/delete),
// so the trail covers the whole lifecycle, not just brokered use.
func (b *Broker) Record(action, secret, detail string) { b.audit(action, secret, detail) }

// audit records what was done with which secret — never the value. Failures are
// swallowed: an unwritable audit row must not block the operation, and the
// operation itself is the thing the user asked for.
func (b *Broker) audit(action, secret, detail string) {
	_ = b.DB.Exec("INSERT INTO audit(ts,action,secret,detail) VALUES(?,?,?,?)",
		Now().Format(time.RFC3339), action, secret, detail)
}

// Audit returns the log. It reveals secret names, grantees and brokered URLs,
// so it is gated on an unlocked vault.
func (b *Broker) Audit(limit int) ([]map[string]any, error) {
	if !b.Vault.IsUnlocked() {
		return nil, ErrLocked
	}
	if limit <= 0 {
		limit = 200
	}
	rows, err := b.DB.Query(
		"SELECT ts, action, secret, detail FROM audit ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var ts, action, secret, detail string
		if err := rows.Scan(&ts, &action, &secret, &detail); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"ts": ts, "action": action, "secret": secret, "detail": detail})
	}
	return out, nil
}

func shortToken(t string) string {
	if len(t) > 8 {
		return t[:8] + "…"
	}
	return t
}
