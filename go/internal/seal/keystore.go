// Package seal holds sealed memory facts and the tombstones that stop a
// forgotten fact from coming back.
//
// A sealed fact's text never reaches the note file, the full-text index or an
// export. The text is encrypted under a random per-fact data key, and that key
// is wrapped by the vault's session key, which exists only while the vault is
// unlocked. Both layers use the existing Fernet primitives in internal/crypto:
// no new algorithm is introduced here.
//
// Forgetting a sealed fact destroys its wrapped data key (crypto-shred). Every
// copy of the ciphertext, in a backup, an old git commit or a sync peer's
// history, is then unreadable, because the key that opens it no longer exists
// anywhere. That only holds while the key store is not itself in the copies:
// see MEMORY-SEAL.md for the key-store location flag.
package seal

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/JeremiahM37/grimoire/go/internal/crypto"
)

// ErrShredded is returned for a sealed fact whose data key has been destroyed.
// It is also what an unknown key id returns, so a key id names nothing after a
// shred, and the caller cannot tell the two apart.
var ErrShredded = errors.New("sealed fact key is gone")

// Keystore is the file of wrapped per-fact data keys.
type Keystore struct {
	Path string
	mu   sync.Mutex
}

// keystoreFile is the on-disk shape. Values are wrapped keys: the base64 data
// key, sealed under the session key.
type keystoreFile struct {
	Keys map[string]string `json:"keys"`
}

// NewKeystore returns the key store at path. The file is created on first use.
func NewKeystore(path string) *Keystore { return &Keystore{Path: path} }

// Seal encrypts plaintext under a fresh data key, stores that key wrapped by
// master, and returns the key's id and the ciphertext. master must be the
// unlocked vault session key.
func (k *Keystore) Seal(master []byte, plaintext string) (keyID, token string, err error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	dataKey := []byte(base64.URLEncoding.EncodeToString(raw))
	idRaw := make([]byte, 12)
	if _, err := rand.Read(idRaw); err != nil {
		return "", "", err
	}
	keyID = hex.EncodeToString(idRaw)

	wrapped, err := crypto.Seal(master, dataKey)
	if err != nil {
		return "", "", err
	}
	ct, err := crypto.Seal(dataKey, []byte(plaintext))
	if err != nil {
		return "", "", err
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return "", "", err
	}
	kf.Keys[keyID] = string(wrapped)
	if err := k.save(kf); err != nil {
		return "", "", err
	}
	return keyID, string(ct), nil
}

// Open decrypts a sealed fact. A destroyed key returns ErrShredded.
func (k *Keystore) Open(master []byte, keyID, token string) (string, error) {
	k.mu.Lock()
	kf, err := k.load()
	k.mu.Unlock()
	if err != nil {
		return "", err
	}
	wrapped, ok := kf.Keys[keyID]
	if !ok {
		return "", ErrShredded
	}
	dataKey, err := crypto.Unseal(master, []byte(wrapped))
	if err != nil {
		return "", fmt.Errorf("unwrapping sealed key: %w", err)
	}
	pt, err := crypto.Unseal(dataKey, []byte(token))
	if err != nil {
		return "", fmt.Errorf("opening sealed fact: %w", err)
	}
	return string(pt), nil
}

// Shred destroys one data key. Destroying an absent key is not an error, so a
// repeated forget succeeds.
func (k *Keystore) Shred(keyID string) error {
	if keyID == "" {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return err
	}
	if _, ok := kf.Keys[keyID]; !ok {
		return nil
	}
	delete(kf.Keys, keyID)
	return k.save(kf)
}

// Has reports whether a data key still exists.
func (k *Keystore) Has(keyID string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return false
	}
	_, ok := kf.Keys[keyID]
	return ok
}

func (k *Keystore) load() (keystoreFile, error) {
	kf := keystoreFile{Keys: map[string]string{}}
	raw, err := os.ReadFile(k.Path)
	if os.IsNotExist(err) {
		return kf, nil
	}
	if err != nil {
		return kf, err
	}
	if err := json.Unmarshal(raw, &kf); err != nil {
		return kf, fmt.Errorf("sealed key store is unreadable: %w", err)
	}
	if kf.Keys == nil {
		kf.Keys = map[string]string{}
	}
	return kf, nil
}

// save writes atomically at 0600. The file holds wrapped keys only, but there
// is no reason for another local user to copy them.
func (k *Keystore) save(kf keystoreFile) error {
	if err := os.MkdirAll(filepath.Dir(k.Path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(kf, "", "  ")
	if err != nil {
		return err
	}
	tmp := k.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, k.Path)
}
