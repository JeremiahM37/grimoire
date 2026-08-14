package secrets

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

func testVault(t *testing.T) (*Vault, *Broker) {
	t.Helper()
	dir := t.TempDir()
	v := New(dir)
	database, err := db.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return v, NewBroker(v, database)
}

func TestInitUnlockLockCycle(t *testing.T) {
	v, _ := testVault(t)
	if v.IsInitialized() {
		t.Fatal("fresh vault reports initialized")
	}
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if !v.IsInitialized() || !v.IsUnlocked() {
		t.Fatal("vault should be initialized and unlocked after init")
	}
	if err := v.Initialize("again"); err != ErrAlreadyInit {
		t.Errorf("re-init = %v, want ErrAlreadyInit", err)
	}
	v.Lock()
	if v.IsUnlocked() {
		t.Fatal("still unlocked after Lock")
	}
	if err := v.Unlock("wrong passphrase"); err != ErrPassphrase {
		t.Errorf("wrong passphrase = %v", err)
	}
	if err := v.Unlock("correct horse battery"); err != nil {
		t.Fatalf("unlock: %v", err)
	}
}

func TestShortPassphraseRejected(t *testing.T) {
	v, _ := testVault(t)
	if err := v.Initialize("short"); err == nil {
		t.Error("a 5-character passphrase was accepted")
	}
}

// The store must be unreadable while locked — that is the whole point of
// dropping the key rather than caching it.
func TestSecretsUnreadableWhileLocked(t *testing.T) {
	v, _ := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "s3cr3t", nil); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	if _, err := v.ListNames(); err != ErrLocked {
		t.Errorf("ListNames while locked = %v, want ErrLocked", err)
	}
	if _, err := v.Get("api-key"); err != ErrLocked {
		t.Errorf("Get while locked = %v, want ErrLocked", err)
	}
	if _, err := v.SealText("x"); err != ErrLocked {
		t.Errorf("SealText while locked = %v", err)
	}
}

func TestSecretsRoundTripAndDelete(t *testing.T) {
	v, _ := testVault(t)
	v.Initialize("correct horse battery")
	if err := v.Put("api-key", "s3cr3t", map[string]any{"note": "prod"}); err != nil {
		t.Fatal(err)
	}
	got, err := v.Get("api-key")
	if err != nil || got != "s3cr3t" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	names, _ := v.ListNames()
	if len(names) != 1 || names[0] != "api-key" {
		t.Errorf("names = %v", names)
	}
	if err := v.Delete("api-key"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("api-key"); err == nil {
		t.Error("deleted secret still readable")
	}
}

// Secrets must survive a lock/unlock cycle — they are sealed on disk, not held
// only in memory.
func TestSecretsPersistAcrossLock(t *testing.T) {
	v, _ := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("token", "abc123", nil)
	v.Lock()
	if err := v.Unlock("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Get("token"); got != "abc123" {
		t.Errorf("after unlock Get = %q", got)
	}
}

func TestIdleLockDropsTheKey(t *testing.T) {
	v, _ := testVault(t)
	v.IdleLock = 50 * time.Millisecond
	v.Initialize("correct horse battery")
	if !v.IsUnlocked() {
		t.Fatal("should start unlocked")
	}
	old := Now
	Now = func() time.Time { return old().Add(time.Second) }
	defer func() { Now = old }()
	if v.IsUnlocked() {
		t.Error("vault should auto-lock after the idle window")
	}
}

// Repeated wrong guesses must get slower, so the on-disk blob is not a free
// oracle for an online attacker.
func TestBruteForceBackoff(t *testing.T) {
	v, _ := testVault(t)
	v.Initialize("correct horse battery")
	v.Lock()
	for i := 0; i < MaxFailures; i++ {
		if err := v.Unlock("nope"); err != ErrPassphrase {
			t.Fatalf("attempt %d = %v", i, err)
		}
	}
	err := v.Unlock("correct horse battery")
	if err == nil || !strings.Contains(err.Error(), "too many attempts") {
		t.Errorf("after %d failures the vault should be locked out, got %v", MaxFailures, err)
	}
}

func TestSealUnsealNoteBody(t *testing.T) {
	v, _ := testVault(t)
	v.Initialize("correct horse battery")
	sealed, err := v.SealText("# Secret\n\nplaintext body\n")
	if err != nil {
		t.Fatal(err)
	}
	if !IsEncrypted(sealed) || strings.Contains(sealed, "plaintext body") {
		t.Fatalf("body not sealed: %q", sealed[:60])
	}
	back, err := v.UnsealText(sealed)
	if err != nil || !strings.Contains(back, "plaintext body") {
		t.Errorf("unseal = %q, %v", back, err)
	}
}

// The headline guarantee: the caller gets the response, never the credential.
func TestBrokerInjectsSecretWithoutRevealingIt(t *testing.T) {
	v, b := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("api-key", "super-secret-value", nil)

	var seenAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("X-Api-Key")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	token, err := b.Grant("api-key", "agent", ts.URL, 900)
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Use(token, "GET", ts.URL+"/thing", "X-Api-Key", "")
	if err != nil {
		t.Fatal(err)
	}
	if seenAuth != "super-secret-value" {
		t.Errorf("target saw header %q, want the secret injected verbatim", seenAuth)
	}
	body := res["body"].(string)
	if strings.Contains(body, "super-secret-value") {
		t.Error("the secret leaked into the brokered response")
	}
	// and the grant listing must never carry values
	grants, _ := b.List()
	for _, g := range grants {
		if strings.Contains(g.Secret+g.Grantee+g.Scope, "super-secret-value") {
			t.Error("grant listing leaked a secret value")
		}
	}
}

// The scope is what stops a token for one API being redirected at another.
func TestBrokerEnforcesScope(t *testing.T) {
	v, b := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("api-key", "value", nil)
	token, _ := b.Grant("api-key", "agent", "https://allowed.example.com", 900)

	if _, err := b.Use(token, "GET", "https://evil.example.com/steal", "Authorization", ""); err == nil {
		t.Error("a url outside the grant scope was allowed")
	}
}

func TestBrokerRejectsExpiredAndUnknownGrants(t *testing.T) {
	v, b := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("api-key", "value", nil)

	if _, err := b.Use("no-such-token", "GET", "https://x", "Authorization", ""); err == nil {
		t.Error("unknown grant was accepted")
	}
	token, _ := b.Grant("api-key", "agent", "", 1)
	old := Now
	Now = func() time.Time { return old().Add(10 * time.Second) }
	defer func() { Now = old }()
	if _, err := b.Use(token, "GET", "https://x", "Authorization", ""); err == nil {
		t.Error("expired grant was accepted")
	}
}

func TestGrantAndAuditRequireUnlock(t *testing.T) {
	v, b := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("api-key", "value", nil)
	v.Lock()
	if _, err := b.Grant("api-key", "agent", "", 900); err != ErrLocked {
		t.Errorf("Grant while locked = %v", err)
	}
	if _, err := b.List(); err != ErrLocked {
		t.Errorf("List while locked = %v", err)
	}
	if _, err := b.Audit(10); err != ErrLocked {
		t.Errorf("Audit while locked = %v", err)
	}
}

// The audit log must record the action without ever storing the value.
func TestAuditRecordsWithoutValues(t *testing.T) {
	v, b := testVault(t)
	v.Initialize("correct horse battery")
	v.Put("api-key", "super-secret-value", nil)
	b.Grant("api-key", "agent", "https://x", 900)

	entries, err := b.Audit(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("nothing audited")
	}
	for _, e := range entries {
		for _, f := range e {
			if s, ok := f.(string); ok && strings.Contains(s, "super-secret-value") {
				t.Errorf("audit log leaked a secret value: %v", e)
			}
		}
	}
}
