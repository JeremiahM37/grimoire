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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
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
//
// Every read-modify-write of one file is serialised by a process-wide lock
// keyed on the file's path, so two Keystore values for the same path (the
// server builds one per call) cannot lose each other's writes, and a Shred
// cannot be undone by a Seal that read the file before it.
type Keystore struct {
	Path string
}

// keystoreFile is the on-disk shape. Keys holds wrapped data keys: the base64
// data key, sealed under a key derived from the session key. Owners binds each
// key id to the account that sealed it, so the owner is not a field a person
// can edit in a note.
type keystoreFile struct {
	Keys   map[string]string `json:"keys"`
	Owners map[string]string `json:"owners,omitempty"`
}

// pathLocks serialises access to one file per path, across Keystore values.
var pathLocks sync.Map

func lockFor(path string) *sync.Mutex {
	m, _ := pathLocks.LoadOrStore(filepath.Clean(path), &sync.Mutex{})
	return m.(*sync.Mutex)
}

// wrapLabel domain-separates the key that wraps data keys from every other
// use of the vault session key (note encryption, the tombstone MAC).
const wrapLabel = "grimoire/seal/wrap/v1"

// wrapKey derives the Fernet key that wraps data keys from the session key.
func wrapKey(master []byte) []byte {
	m := hmac.New(sha256.New, master)
	m.Write([]byte(wrapLabel))
	return []byte(base64.URLEncoding.EncodeToString(m.Sum(nil)))
}

// NewKeystore returns the key store at path. The file is created on first use.
func NewKeystore(path string) *Keystore { return &Keystore{Path: path} }

// Seal encrypts plaintext under a fresh data key, stores that key wrapped by
// master, records owner as the only account Open will serve, and returns the
// key's id and the ciphertext. master must be the unlocked vault session key.
func (k *Keystore) Seal(master []byte, owner, plaintext string) (keyID, token string, err error) {
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

	wrapped, err := crypto.Seal(wrapKey(master), dataKey)
	if err != nil {
		return "", "", err
	}
	ct, err := crypto.Seal(dataKey, []byte(plaintext))
	if err != nil {
		return "", "", err
	}

	mu := lockFor(k.Path)
	mu.Lock()
	defer mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return "", "", err
	}
	kf.Keys[keyID] = string(wrapped)
	kf.Owners[keyID] = owner
	if err := k.save(kf); err != nil {
		return "", "", err
	}
	return keyID, string(ct), nil
}

// Open decrypts a sealed fact for owner. A destroyed key, and a key that was
// sealed by a different owner, both return ErrShredded, so a copy of a bullet
// pasted into another account's note opens nothing and says nothing about why.
func (k *Keystore) Open(master []byte, owner, keyID, token string) (string, error) {
	mu := lockFor(k.Path)
	mu.Lock()
	kf, err := k.load()
	mu.Unlock()
	if err != nil {
		return "", err
	}
	wrapped, ok := kf.Keys[keyID]
	if !ok || owner == "" || kf.Owners[keyID] != owner {
		return "", ErrShredded
	}
	dataKey, err := crypto.Unseal(wrapKey(master), []byte(wrapped))
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
	mu := lockFor(k.Path)
	mu.Lock()
	defer mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return err
	}
	if _, ok := kf.Keys[keyID]; !ok {
		return nil
	}
	delete(kf.Keys, keyID)
	delete(kf.Owners, keyID)
	return k.save(kf)
}

// Has reports whether a data key still exists.
func (k *Keystore) Has(keyID string) bool {
	mu := lockFor(k.Path)
	mu.Lock()
	defer mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return false
	}
	_, ok := kf.Keys[keyID]
	return ok
}

func (k *Keystore) load() (keystoreFile, error) {
	kf := keystoreFile{Keys: map[string]string{}, Owners: map[string]string{}}
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
	if kf.Owners == nil {
		kf.Owners = map[string]string{}
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
	return writeAtomic(k.Path, raw)
}

// Rewrap moves every wrapped data key from oldMaster to newMaster, for a
// passphrase change. The data keys themselves do not change, so ciphertext
// stays valid. It is all-or-nothing: the file is written only if every key
// unwraps under oldMaster.
func (k *Keystore) Rewrap(oldMaster, newMaster []byte) error {
	mu := lockFor(k.Path)
	mu.Lock()
	defer mu.Unlock()
	kf, err := k.load()
	if err != nil {
		return err
	}
	out := make(map[string]string, len(kf.Keys))
	for id, wrapped := range kf.Keys {
		dataKey, err := crypto.Unseal(wrapKey(oldMaster), []byte(wrapped))
		if err != nil {
			return fmt.Errorf("unwrapping sealed key %s: %w", id, err)
		}
		re, err := crypto.Seal(wrapKey(newMaster), dataKey)
		if err != nil {
			return err
		}
		out[id] = string(re)
	}
	if len(out) == 0 {
		return nil
	}
	return k.save(keystoreFile{Keys: out, Owners: kf.Owners})
}

// writeAtomic replaces path with data at 0600 through a uniquely named temp
// file in the same directory, so concurrent writers never share a temp path,
// and a crash leaves either the old file or the new one.
func writeAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
