// Package auth is identity: who is asking.
//
// Grimoire began as one person's context server, where "who is asking" had one
// answer and an optional shared token was a reasonable gate. Sharing it with a
// team makes that answer wrong in two directions at once: everyone holding the
// same token means nobody can be told apart in an audit log, and one visibility
// bit per note means the only sharing model is all-or-nothing.
//
// So identity is real here — accounts, sessions, API keys — and authorization
// lives next to it in spaces.go, which decides what an identity may read.
//
// The single-user deployment must not pay for any of this. With no accounts
// created the server behaves exactly as it did: no login, everything visible,
// the optional shared token still honoured. Multi-user begins the moment an
// administrator exists, and not before — see Enabled.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// Roles. A member reads and writes the spaces they belong to; an admin also
// manages accounts, spaces, connectors and the credential vault.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

var (
	ErrNoSuchUser    = errors.New("no such user")
	ErrBadPassword   = errors.New("wrong password")
	ErrNameTaken     = errors.New("that name is already taken")
	ErrInvalidName   = errors.New("names may contain letters, numbers, dot, dash and underscore")
	ErrWeakPassword  = errors.New("password must be at least 10 characters")
	ErrSessionExpiry = errors.New("session expired")
)

// Now is indirected for tests.
var Now = func() time.Time { return time.Now() }

// SessionTTL is how long a browser session lasts without re-authenticating.
const SessionTTL = 14 * 24 * time.Hour

// User is an account. The hash never leaves this package's queries.
type User struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Display string `json:"display"`
	Role    string `json:"role"`
	Created string `json:"created"`
}

func (u User) IsAdmin() bool { return u.Role == RoleAdmin }

// Store owns accounts, sessions and API keys.
type Store struct{ DB *db.DB }

func New(database *db.DB) *Store { return &Store{DB: database} }

// Enabled reports whether this deployment has accounts at all.
//
// Everything that follows keys off this: with no accounts the server is the
// single-user one it has always been, and turning multi-user on is creating
// the first administrator rather than setting a flag. That ordering matters —
// a flag could be set on a running server and lock its owner out of their own
// notes; creating an account cannot.
func (s *Store) Enabled() bool {
	n, err := s.DB.Count("SELECT count(*) FROM users")
	return err == nil && n > 0
}

// Create adds an account. The first account is always an administrator: a
// deployment whose only user cannot manage it is a deployment nobody can fix.
func (s *Store) Create(name, display, password, role string) (User, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if !validName(name) {
		return User{}, ErrInvalidName
	}
	if len([]rune(password)) < 10 {
		return User{}, ErrWeakPassword
	}
	taken, err := s.DB.Count("SELECT count(*) FROM users WHERE name=?", name)
	if err != nil {
		return User{}, err
	}
	if taken > 0 {
		return User{}, ErrNameTaken
	}
	if !s.Enabled() {
		role = RoleAdmin
	}
	if role != RoleAdmin {
		role = RoleMember
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, err
	}
	u := User{ID: newID(), Name: name, Display: strings.TrimSpace(display), Role: role,
		Created: Now().UTC().Format(time.RFC3339)}
	if u.Display == "" {
		u.Display = name
	}
	if err := s.DB.Exec(
		"INSERT INTO users(id,name,display,pwhash,role,created) VALUES(?,?,?,?,?,?)",
		u.ID, u.Name, u.Display, hash, u.Role, u.Created); err != nil {
		return User{}, err
	}
	return u, nil
}

// Authenticate checks a password and returns the account.
//
// A wrong name and a wrong password are reported the same way and cost the
// same work: skipping the hash for an unknown name turns the login endpoint
// into a fast oracle for which accounts exist.
func (s *Store) Authenticate(name, password string) (User, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	var u User
	var hash string
	err := s.DB.QueryRow(
		"SELECT id,name,display,role,created,pwhash FROM users WHERE name=?", name).
		Scan(&u.ID, &u.Name, &u.Display, &u.Role, &u.Created, &hash)
	if err != nil {
		// Hash anyway, against a dummy of the same shape.
		_, _ = HashPassword(password)
		return User{}, ErrBadPassword
	}
	ok, err := VerifyPassword(hash, password)
	if err != nil || !ok {
		return User{}, ErrBadPassword
	}
	return u, nil
}

// Get returns one account by id.
func (s *Store) Get(id string) (User, error) {
	var u User
	err := s.DB.QueryRow(
		"SELECT id,name,display,role,created FROM users WHERE id=?", id).
		Scan(&u.ID, &u.Name, &u.Display, &u.Role, &u.Created)
	if err != nil {
		return User{}, ErrNoSuchUser
	}
	return u, nil
}

// ByName returns one account by login name.
func (s *Store) ByName(name string) (User, error) {
	var u User
	err := s.DB.QueryRow(
		"SELECT id,name,display,role,created FROM users WHERE name=?",
		strings.ToLower(strings.TrimSpace(name))).
		Scan(&u.ID, &u.Name, &u.Display, &u.Role, &u.Created)
	if err != nil {
		return User{}, ErrNoSuchUser
	}
	return u, nil
}

// List returns every account, oldest first.
func (s *Store) List() ([]User, error) {
	rows, err := s.DB.Query("SELECT id,name,display,role,created FROM users ORDER BY created, name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Name, &u.Display, &u.Role, &u.Created); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetPassword changes an account's password and invalidates its sessions,
// which is the point of changing it.
func (s *Store) SetPassword(id, password string) error {
	if len([]rune(password)) < 10 {
		return ErrWeakPassword
	}
	hash, err := HashPassword(password)
	if err != nil {
		return err
	}
	if err := s.DB.Exec("UPDATE users SET pwhash=? WHERE id=?", hash, id); err != nil {
		return err
	}
	return s.DB.Exec("DELETE FROM sessions WHERE user=?", id)
}

// SetRole promotes or demotes an account. The last administrator cannot be
// demoted: an instance with no admin can no longer be administered.
func (s *Store) SetRole(id, role string) error {
	if role != RoleAdmin {
		role = RoleMember
	}
	if role == RoleMember {
		admins, err := s.DB.Count("SELECT count(*) FROM users WHERE role=? AND id<>?", RoleAdmin, id)
		if err != nil {
			return err
		}
		if admins == 0 {
			return errors.New("this is the only administrator")
		}
	}
	return s.DB.Exec("UPDATE users SET role=? WHERE id=?", role, id)
}

// Delete removes an account, its sessions and its keys. Same last-admin rule.
func (s *Store) Delete(id string) error {
	u, err := s.Get(id)
	if err != nil {
		return err
	}
	if u.IsAdmin() {
		admins, err := s.DB.Count("SELECT count(*) FROM users WHERE role=? AND id<>?", RoleAdmin, id)
		if err != nil {
			return err
		}
		if admins == 0 {
			return errors.New("this is the only administrator")
		}
	}
	for _, q := range []string{
		"DELETE FROM sessions WHERE user=?",
		"DELETE FROM api_keys WHERE user=?",
		"DELETE FROM space_members WHERE user=?",
		"DELETE FROM users WHERE id=?",
	} {
		if err := s.DB.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------- sessions

// StartSession issues a browser session token.
func (s *Store) StartSession(userID string) (string, error) {
	token := randomToken()
	expires := float64(Now().Add(SessionTTL).Unix())
	if err := s.DB.Exec(
		"INSERT INTO sessions(token,user,expires,created) VALUES(?,?,?,?)",
		hashToken(token), userID, expires, Now().UTC().Format(time.RFC3339)); err != nil {
		return "", err
	}
	return token, nil
}

// UserForSession resolves a session token, dropping it if it has expired.
//
// Tokens are stored hashed. A session table read from a backup or a stray
// index copy would otherwise hand over live sessions for every account.
func (s *Store) UserForSession(token string) (User, error) {
	var userID string
	var expires float64
	err := s.DB.QueryRow("SELECT user,expires FROM sessions WHERE token=?", hashToken(token)).
		Scan(&userID, &expires)
	if err != nil {
		return User{}, ErrNoSuchUser
	}
	if float64(Now().Unix()) > expires {
		_ = s.DB.Exec("DELETE FROM sessions WHERE token=?", hashToken(token))
		return User{}, ErrSessionExpiry
	}
	return s.Get(userID)
}

// EndSession logs one session out.
func (s *Store) EndSession(token string) error {
	return s.DB.Exec("DELETE FROM sessions WHERE token=?", hashToken(token))
}

// ---------------------------------------------------------------- api keys

// APIKey is what an agent authenticates with. The value is shown once, at
// creation, and stored only as a hash.
type APIKey struct {
	ID       string `json:"id"`
	User     string `json:"user"`
	Label    string `json:"label"`
	Created  string `json:"created"`
	LastUsed string `json:"last_used"`
}

// CreateAPIKey returns the key value and its record. The value cannot be
// recovered later.
func (s *Store) CreateAPIKey(userID, label string) (string, APIKey, error) {
	key := "gk_" + randomToken()
	rec := APIKey{ID: newID(), User: userID, Label: strings.TrimSpace(label),
		Created: Now().UTC().Format(time.RFC3339)}
	if rec.Label == "" {
		rec.Label = "agent"
	}
	if err := s.DB.Exec(
		"INSERT INTO api_keys(id,hash,user,label,created,last_used) VALUES(?,?,?,?,?,'')",
		rec.ID, hashToken(key), rec.User, rec.Label, rec.Created); err != nil {
		return "", APIKey{}, err
	}
	return key, rec, nil
}

// UserForAPIKey resolves an API key to its owner.
func (s *Store) UserForAPIKey(key string) (User, error) {
	var userID string
	if err := s.DB.QueryRow("SELECT user FROM api_keys WHERE hash=?", hashToken(key)).
		Scan(&userID); err != nil {
		return User{}, ErrNoSuchUser
	}
	_ = s.DB.Exec("UPDATE api_keys SET last_used=? WHERE hash=?",
		Now().UTC().Format(time.RFC3339), hashToken(key))
	return s.Get(userID)
}

// ListAPIKeys returns one account's keys, never their values.
func (s *Store) ListAPIKeys(userID string) ([]APIKey, error) {
	rows, err := s.DB.Query(
		"SELECT id,user,label,created,last_used FROM api_keys WHERE user=? ORDER BY created", userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.User, &k.Label, &k.Created, &k.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deletes one key.
func (s *Store) RevokeAPIKey(userID, id string) error {
	return s.DB.Exec("DELETE FROM api_keys WHERE id=? AND user=?", id, userID)
}

// ---------------------------------------------------------------- passwords

// HashPassword derives an Argon2id hash in the encoded form used by the rest
// of the ecosystem, so an operator can recognize it.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return encodeHash(password, salt, argonTime, argonMemory, argonThreads), nil
}

// VerifyPassword checks a password against an encoded hash in constant time.
func VerifyPassword(encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, fmt.Errorf("unrecognized password hash")
	}
	var mem, time uint32
	var threads uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &mem, &time, &threads); err != nil {
		return false, err
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false, err
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false, err
	}
	got := argon2.IDKey([]byte(password), salt, time, mem, threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// Argon2id parameters for passwords. Lighter than the secret vault's, on
// purpose: the vault derives a key once, interactively, while this runs on
// every login and every API call that is not already a session.
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

func encodeHash(password string, salt []byte, t, m uint32, p uint8) string {
	sum := argon2.IDKey([]byte(password), salt, t, m, p, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", m, t, p,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(sum))
}

// ---------------------------------------------------------------- helpers

func validName(name string) bool {
	if len(name) < 2 || len(name) > 40 {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// hashToken is what goes in the database for session and API-key tokens.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
