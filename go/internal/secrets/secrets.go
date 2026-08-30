// Package secrets is the credential vault and USE-not-READ broker.
//
// Port of server/secrets.py. The design promise: an agent can USE a credential
// without ever seeing it. The value is injected server-side into an outbound
// request; it never enters the agent's context. Everything here exists to keep
// that promise, so the defensive details are the feature, not overhead:
//
//   - the derived key lives ONLY in memory, never on disk, and is dropped on
//     lock and after an idle timeout;
//   - a wrong passphrase backs off exponentially, so the on-disk blob is not a
//     free offline oracle for a fast online guesser;
//   - grants carry a scope and an expiry, and list_grants never returns values.
package secrets

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/crypto"
)

// EncPrefix marks an encrypted note body on disk.
const EncPrefix = "grimoire:enc:v1:"

// verifierToken is sealed at init so unlock can validate a passphrase without
// decrypting the whole payload.
const verifierToken = "grimoire-vault-v1"

// MaxFailures before the exponential lockout starts.
const MaxFailures = 5

var (
	ErrLocked      = errors.New("vault is locked")
	ErrNotInit     = errors.New("vault not initialized")
	ErrAlreadyInit = errors.New("vault already initialized")
	ErrPassphrase  = errors.New("wrong passphrase")
)

// Now is indirected for tests.
var Now = func() time.Time { return time.Now() }

// Vault holds the sealed store and the in-memory session key.
type Vault struct {
	Path string
	// IdleLock drops the key after this long without a sensitive operation,
	// shrinking the window in which a compromised process can read secrets.
	IdleLock time.Duration

	mu           sync.Mutex
	key          []byte
	lastActivity time.Time
	failures     int
	lockUntil    time.Time
}

func New(grimoireDir string) *Vault {
	idle := 900 * time.Second
	if v := os.Getenv("GRIMOIRE_VAULT_IDLE_LOCK"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			idle = time.Duration(n) * time.Second
		}
	}
	return &Vault{Path: filepath.Join(grimoireDir, "secrets.json"), IdleLock: idle}
}

type blob struct {
	Salt     string `json:"salt"`
	Verifier string `json:"verifier"`
	KDF      string `json:"kdf"`
	Secrets  string `json:"secrets"`
	Grants   string `json:"grants"`
}

func (v *Vault) loadBlob() blob {
	var b blob
	raw, err := os.ReadFile(v.Path)
	if err != nil {
		return b
	}
	_ = json.Unmarshal(raw, &b)
	return b
}

func (v *Vault) saveBlob(b blob) error {
	if err := os.MkdirAll(filepath.Dir(v.Path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	tmp := v.Path + ".tmp"
	// 0600: the sealed blob is not readable without the passphrase, but there
	// is no reason to let other local users copy it for offline attack
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, v.Path)
}

// IsInitialized reports whether a vault exists on disk.
func (v *Vault) IsInitialized() bool { return v.loadBlob().Salt != "" }

// IsUnlocked reports whether the session key is held, applying the idle timeout.
func (v *Vault) IsUnlocked() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.unlockedLocked()
}

func (v *Vault) unlockedLocked() bool {
	if v.key == nil {
		return false
	}
	if v.IdleLock > 0 && Now().Sub(v.lastActivity) > v.IdleLock {
		v.key = nil // auto-lock after idle
		return false
	}
	return true
}

// Status is the console's lock indicator.
func (v *Vault) Status() map[string]any {
	out := map[string]any{
		"initialized": v.IsInitialized(),
		"unlocked":    v.IsUnlocked(),
		"count":       nil,
	}
	if out["unlocked"].(bool) {
		if names, err := v.ListNames(); err == nil {
			out["count"] = len(names)
		}
	}
	return out
}

func (v *Vault) checkLockoutLocked() error {
	if remaining := v.lockUntil.Sub(Now()); remaining > 0 {
		return fmt.Errorf("too many attempts — locked for %ds", int(remaining.Seconds())+1)
	}
	return nil
}

func (v *Vault) recordFailureLocked() {
	v.failures++
	if v.failures >= MaxFailures {
		// exponential backoff: 30s, 60s, 120s … capped at an hour
		backoff := 30 * math.Pow(2, float64(v.failures-MaxFailures))
		if backoff > 3600 {
			backoff = 3600
		}
		v.lockUntil = Now().Add(time.Duration(backoff) * time.Second)
	}
}

// Initialize creates the vault and holds it unlocked.
func (v *Vault) Initialize(passphrase string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.loadBlob().Salt != "" {
		return ErrAlreadyInit
	}
	if len(passphrase) < 8 {
		return errors.New("passphrase must be at least 8 characters")
	}
	salt, err := crypto.NewSalt()
	if err != nil {
		return err
	}
	key, err := crypto.DeriveKey(passphrase, salt, crypto.DefaultKDF)
	if err != nil {
		return err
	}
	verifier, err := crypto.Seal(key, []byte(verifierToken))
	if err != nil {
		return err
	}
	b := blob{
		Salt:     base64.StdEncoding.EncodeToString(salt),
		Verifier: base64.StdEncoding.EncodeToString(verifier),
		KDF:      crypto.DefaultKDF,
	}
	if err := v.saveBlob(b); err != nil {
		return err
	}
	v.key = key
	v.lastActivity = Now()
	return v.writePayloadLocked(map[string]secretEntry{})
}

// Unlock derives the key and validates it against the verifier.
func (v *Vault) Unlock(passphrase string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.checkLockoutLocked(); err != nil {
		return err
	}
	b := v.loadBlob()
	if b.Salt == "" {
		return ErrNotInit
	}
	salt, err := base64.StdEncoding.DecodeString(b.Salt)
	if err != nil {
		return err
	}
	kdf := b.KDF
	if kdf == "" {
		kdf = "pbkdf2" // legacy vaults predate the kdf field
	}
	key, err := crypto.DeriveKey(passphrase, salt, kdf)
	if err != nil {
		return err
	}
	verifier, err := base64.StdEncoding.DecodeString(b.Verifier)
	if err != nil {
		return err
	}
	if _, err := crypto.Unseal(key, verifier); err != nil {
		v.recordFailureLocked()
		return ErrPassphrase
	}
	v.failures, v.lockUntil = 0, time.Time{}
	v.key = key
	v.lastActivity = Now()
	return nil
}

// Lock drops the session key.
func (v *Vault) Lock() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.key = nil
}

type secretEntry struct {
	Value string         `json:"value"`
	Meta  map[string]any `json:"meta,omitempty"`
	// Versions are the values this secret used to hold, newest first. Sealed
	// with everything else: an old credential is exactly as sensitive as the
	// one that replaced it.
	Versions []Version `json:"versions,omitempty"`
}

func (v *Vault) payloadLocked() (map[string]secretEntry, error) {
	if !v.unlockedLocked() {
		return nil, ErrLocked
	}
	v.lastActivity = Now() // refresh the idle timer on every sensitive operation
	b := v.loadBlob()
	if b.Secrets == "" {
		return map[string]secretEntry{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b.Secrets)
	if err != nil {
		return nil, err
	}
	plain, err := crypto.Unseal(v.key, raw)
	if err != nil {
		return nil, err
	}
	var out map[string]secretEntry
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *Vault) writePayloadLocked(payload map[string]secretEntry) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	sealed, err := crypto.Seal(v.key, raw)
	if err != nil {
		return err
	}
	b := v.loadBlob()
	b.Secrets = base64.StdEncoding.EncodeToString(sealed)
	return v.saveBlob(b)
}

// ListNames returns secret names only — never values.
func (v *Vault) ListNames() ([]string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(payload))
	for name := range payload {
		out = append(out, name)
	}
	return out, nil
}

// Put stores or replaces a secret.
func (v *Vault) Put(name, value string, meta map[string]any) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("name required")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	payload[name] = secretEntry{Value: value, Meta: meta}
	return v.writePayloadLocked(payload)
}

// Delete removes a secret.
func (v *Vault) Delete(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	delete(payload, name)
	return v.writePayloadLocked(payload)
}

// Get returns a secret's value. Callers must never send this to an agent — it
// exists for the broker, which injects it into an outbound request server-side.
func (v *Vault) Get(name string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return "", err
	}
	entry, ok := payload[name]
	if !ok {
		return "", fmt.Errorf("no such secret: %s", name)
	}
	return entry.Value, nil
}

// SealText encrypts a note body under the session key.
func (v *Vault) SealText(plaintext string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.unlockedLocked() {
		return "", ErrLocked
	}
	v.lastActivity = Now()
	tok, err := crypto.Seal(v.key, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return EncPrefix + string(tok), nil
}

// UnsealText decrypts a note body.
func (v *Vault) UnsealText(body string) (string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.unlockedLocked() {
		return "", ErrLocked
	}
	v.lastActivity = Now()
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if !strings.HasPrefix(trimmed, EncPrefix) {
		return body, nil
	}
	plain, err := crypto.Unseal(v.key, []byte(strings.TrimPrefix(trimmed, EncPrefix)))
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// IsEncrypted reports whether a body is sealed.
func IsEncrypted(body string) bool {
	return strings.HasPrefix(strings.TrimLeft(body, " \t\r\n"), EncPrefix)
}

// ChangePassphrase rotates the vault passphrase: verify the old one, re-derive
// with a fresh salt and Argon2id (upgrading a legacy pbkdf2 vault on the way),
// and re-seal the secret store under the new key.
//
// The caller is responsible for re-sealing encrypted NOTE bodies, which it must
// do BEFORE the key is swapped — otherwise a failure part-way leaves notes
// sealed under a key nobody holds.
func (v *Vault) ChangePassphrase(old, next string, reseal func(oldKey, newKey []byte) error) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.checkLockoutLocked(); err != nil {
		return err
	}
	if len(next) < 8 {
		return errors.New("new passphrase must be at least 8 characters")
	}
	b := v.loadBlob()
	if b.Salt == "" {
		return ErrNotInit
	}
	salt, err := base64.StdEncoding.DecodeString(b.Salt)
	if err != nil {
		return err
	}
	kdf := b.KDF
	if kdf == "" {
		kdf = "pbkdf2"
	}
	oldKey, err := crypto.DeriveKey(old, salt, kdf)
	if err != nil {
		return err
	}
	verifier, err := base64.StdEncoding.DecodeString(b.Verifier)
	if err != nil {
		return err
	}
	if _, err := crypto.Unseal(oldKey, verifier); err != nil {
		v.recordFailureLocked()
		return errors.New("wrong current passphrase")
	}
	v.failures, v.lockUntil = 0, time.Time{}

	// read the payload with the OLD key before anything is swapped
	oldSecrets := map[string]secretEntry{}
	if b.Secrets != "" {
		raw, err := base64.StdEncoding.DecodeString(b.Secrets)
		if err != nil {
			return err
		}
		plain, err := crypto.Unseal(oldKey, raw)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(plain, &oldSecrets); err != nil {
			return err
		}
	}

	newSalt, err := crypto.NewSalt()
	if err != nil {
		return err
	}
	newKey, err := crypto.DeriveKey(next, newSalt, crypto.DefaultKDF)
	if err != nil {
		return err
	}
	// re-seal notes first: if this fails, nothing has been swapped yet
	if reseal != nil {
		if err := reseal(oldKey, newKey); err != nil {
			return fmt.Errorf("re-sealing notes: %w", err)
		}
	}
	newVerifier, err := crypto.Seal(newKey, []byte(verifierToken))
	if err != nil {
		return err
	}
	v.key = newKey
	v.lastActivity = Now()
	b.Salt = base64.StdEncoding.EncodeToString(newSalt)
	b.Verifier = base64.StdEncoding.EncodeToString(newVerifier)
	b.KDF = crypto.DefaultKDF
	if err := v.saveBlob(b); err != nil {
		return err
	}
	return v.writePayloadLocked(oldSecrets)
}

// ResealWith re-encrypts a body from one key to another, used during rotation.
func ResealWith(oldKey, newKey []byte, body string) (string, error) {
	trimmed := strings.TrimLeft(body, " \t\r\n")
	if !strings.HasPrefix(trimmed, EncPrefix) {
		return body, nil
	}
	plain, err := crypto.Unseal(oldKey, []byte(strings.TrimPrefix(trimmed, EncPrefix)))
	if err != nil {
		return "", err
	}
	tok, err := crypto.Seal(newKey, plain)
	if err != nil {
		return "", err
	}
	return EncPrefix + string(tok), nil
}
