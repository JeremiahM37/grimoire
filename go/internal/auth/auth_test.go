package auth

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

// The single-user deployment must not change until someone deliberately makes
// it multi-user, and the act that does so is creating an account — not setting
// a flag that could be flipped on a running server by accident.
func TestMultiUserBeginsWithTheFirstAccount(t *testing.T) {
	s := testStore(t)
	if s.Enabled() {
		t.Fatal("a fresh instance reports accounts")
	}
	if !Unrestricted().IsAdmin() || !Unrestricted().CanRead("anything") {
		t.Fatal("the no-accounts principal is restricted")
	}
	u, err := s.Create("alice", "Alice", "correct horse battery", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if !u.IsAdmin() {
		t.Fatal("the first account must be an administrator, whatever role was asked for")
	}
	if !s.Enabled() {
		t.Fatal("accounts exist but Enabled() is false")
	}
	second, err := s.Create("bob", "Bob", "correct horse battery", RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	if second.IsAdmin() {
		t.Fatal("a later account was silently made an administrator")
	}
}

func TestPasswordsAreHashedAndVerified(t *testing.T) {
	s := testStore(t)
	if _, err := s.Create("alice", "", "short", RoleAdmin); err != ErrWeakPassword {
		t.Fatalf("weak password accepted: %v", err)
	}
	u, err := s.Create("alice", "Alice", "correct horse battery", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.DB.QueryRow("SELECT pwhash FROM users WHERE id=?", u.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == "correct horse battery" || len(stored) < 40 {
		t.Fatalf("password is not hashed: %q", stored)
	}
	if _, err := s.Authenticate("alice", "correct horse battery"); err != nil {
		t.Fatalf("correct password rejected: %v", err)
	}
	if _, err := s.Authenticate("alice", "wrong"); err != ErrBadPassword {
		t.Fatalf("wrong password accepted: %v", err)
	}
	// An unknown name must be indistinguishable from a wrong password.
	if _, err := s.Authenticate("nobody", "whatever"); err != ErrBadPassword {
		t.Fatalf("unknown account reported differently: %v", err)
	}
}

func TestSessionsExpireAndAreStoredHashed(t *testing.T) {
	s := testStore(t)
	u, err := s.Create("alice", "", "correct horse battery", RoleAdmin)
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.StartSession(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.DB.Count("SELECT count(*) FROM sessions WHERE token=?", token)
	if err != nil || n != 0 {
		t.Fatal("the raw session token is stored in the database")
	}
	got, err := s.UserForSession(token)
	if err != nil || got.ID != u.ID {
		t.Fatalf("session did not resolve: %v", err)
	}

	old := Now
	t.Cleanup(func() { Now = old })
	Now = func() time.Time { return old().Add(SessionTTL + time.Hour) }
	if _, err := s.UserForSession(token); err != ErrSessionExpiry {
		t.Fatalf("expired session still resolves: %v", err)
	}
	Now = old
	if _, err := s.UserForSession(token); err == nil {
		t.Fatal("an expired session was not dropped")
	}
}

func TestAPIKeysAuthenticateAndRevoke(t *testing.T) {
	s := testStore(t)
	u, _ := s.Create("agent-owner", "", "correct horse battery", RoleAdmin)
	key, rec, err := s.CreateAPIKey(u.ID, "claude-code")
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.UserForAPIKey(key)
	if err != nil || got.ID != u.ID {
		t.Fatalf("api key did not resolve: %v", err)
	}
	keys, err := s.ListAPIKeys(u.ID)
	if err != nil || len(keys) != 1 {
		t.Fatalf("listing keys: %v %d", err, len(keys))
	}
	if keys[0].LastUsed == "" {
		t.Error("use was not recorded")
	}
	if err := s.RevokeAPIKey(u.ID, rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForAPIKey(key); err == nil {
		t.Fatal("a revoked key still authenticates")
	}
}

func TestTheLastAdministratorCannotBeRemoved(t *testing.T) {
	s := testStore(t)
	admin, _ := s.Create("admin", "", "correct horse battery", RoleAdmin)
	if err := s.SetRole(admin.ID, RoleMember); err == nil {
		t.Fatal("the only administrator was demoted")
	}
	if err := s.Delete(admin.ID); err == nil {
		t.Fatal("the only administrator was deleted")
	}
	second, _ := s.Create("admin2", "", "correct horse battery", RoleMember)
	if err := s.SetRole(second.ID, RoleAdmin); err != nil {
		t.Fatal(err)
	}
	if err := s.SetRole(admin.ID, RoleMember); err != nil {
		t.Fatalf("demotion refused with two administrators: %v", err)
	}
}

func TestChangingAPasswordEndsSessions(t *testing.T) {
	s := testStore(t)
	u, _ := s.Create("alice", "", "correct horse battery", RoleAdmin)
	token, _ := s.StartSession(u.ID)
	if err := s.SetPassword(u.ID, "a different long password"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UserForSession(token); err == nil {
		t.Fatal("a session survived the password change that was meant to end it")
	}
}
