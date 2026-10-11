package api

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/seal"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Sealed facts (vis=sealed) and tombstones. See MEMORY-SEAL.md for the
// contract; the short version is here so the handlers read in context.
//
//   - A sealed fact's text is encrypted before anything else sees it. The
//     bullet keeps only the placeholder, the ciphertext and the key id.
//   - Its owner is the account that wrote it. Only that account, with the
//     vault unlocked, gets the text back, and only from recall with
//     include_private. Everyone else sees the placeholder.
//   - Forgetting a sealed fact destroys its data key and writes a tombstone
//     for its id. The tombstone is what stops a stale sync peer or a restored
//     backup from putting the bullet back.

// sealOwner names the caller for a sealed fact. Anonymous callers have no
// account to own anything, so they cannot seal.
func sealOwner(r *http.Request) (string, bool) {
	p := principal(r)
	switch {
	case p.Anonymous:
		return "", false
	case p.Unrestricted:
		return "local", true
	case p.User.ID != "":
		return "user:" + p.User.ID, true
	}
	return "", false
}

// sealKeys is the key store. GRIMOIRE_SEAL_KEYS moves it out of the vault
// (see MEMORY-SEAL.md). The default is beside the secret vault, inside the
// vault's .grimoire directory, which is not synced but is in the vault tree.
func (s *Server) sealKeys() *seal.Keystore {
	if p := strings.TrimSpace(os.Getenv("GRIMOIRE_SEAL_KEYS")); p != "" {
		return seal.NewKeystore(p)
	}
	return seal.NewKeystore(filepath.Join(s.Vault.Root, ".grimoire", "seal-keys.json"))
}

// sealText encrypts a fact under a new data key. It needs the unlocked vault.
func (s *Server) sealText(text string) (keyID, token string, err error) {
	if s.Secrets == nil {
		return "", "", secrets.ErrLocked
	}
	err = s.Secrets.WithSessionKey(func(key []byte) error {
		var e error
		keyID, token, e = s.sealKeys().Seal(key, text)
		return e
	})
	return keyID, token, err
}

// sealErrStatus maps a sealing failure to a status: a locked vault is a
// request the person can fix by unlocking, anything else is the server's.
func sealErrStatus(err error) int {
	if errors.Is(err, secrets.ErrLocked) {
		return http.StatusLocked
	}
	return http.StatusInternalServerError
}

// revealSealed puts the text of sealed facts back for their owner. Every other
// caller keeps the placeholder the index already holds. A fact that cannot be
// opened (its key was shredded, or the vault is locked) also keeps it: the
// reader is told nothing about why.
func (s *Server) revealSealed(r *http.Request, hits []index.MemoryHit) {
	owner, ok := sealOwner(r)
	if !ok || s.Secrets == nil {
		return
	}
	notes := map[string][]memory.Entry{}
	for i := range hits {
		h := &hits[i]
		if h.Visibility != memory.VisSealed {
			continue
		}
		entries, seen := notes[h.Note]
		if !seen {
			if note, err := s.Vault.Read(h.Note); err == nil {
				entries = memory.Parse(note.Body)
			}
			notes[h.Note] = entries
		}
		for _, e := range entries {
			if e.ID != h.ID || e.Owner != owner || e.Sealed == "" {
				continue
			}
			var plain string
			err := s.Secrets.WithSessionKey(func(key []byte) error {
				var oe error
				plain, oe = s.sealKeys().Open(key, e.KeyID, e.Sealed)
				return oe
			})
			if err == nil {
				h.Text = plain
			}
			break
		}
	}
}

// entryIsSealed reports whether the bullet is a sealed fact. A note or id that
// does not exist is not sealed, so callers fall through to their own errors.
func (s *Server) entryIsSealed(note, id string) bool {
	e, ok := s.entryAt(note, id)
	return ok && e.Visibility == memory.VisSealed
}

func (s *Server) entryAt(note, id string) (memory.Entry, bool) {
	existing, err := s.Vault.Read(note)
	if err != nil {
		return memory.Entry{}, false
	}
	for _, e := range memory.Parse(existing.Body) {
		if e.ID == id {
			return e, true
		}
	}
	return memory.Entry{}, false
}

// forgetSealed is the forget that a sealed fact always gets, whatever the
// request asked for: shred the key, then tombstone the id. It runs after the
// bullet has left the note, so a failure never leaves a live bullet whose key
// is already gone.
func (s *Server) forgetSealed(e memory.Entry, reason string) error {
	if e.KeyID != "" {
		if err := s.sealKeys().Shred(e.KeyID); err != nil {
			return err
		}
	}
	return seal.NewTombLog(s.Vault.Root).Add(e.ID, reason, vault.Now())
}

// tombstoneRemoved records a hard removal, so the id cannot come back from a
// stale copy. Sealed facts also get their key destroyed.
func (s *Server) tombstoneRemoved(e memory.Entry) error {
	if e.Visibility == memory.VisSealed {
		return s.forgetSealed(e, "forget")
	}
	return seal.NewTombLog(s.Vault.Root).Add(e.ID, "forget", vault.Now())
}

// suppressTombstoned removes tombstoned bullets from one memory note that a
// sync or a restore just wrote. The rewrite is a snapshot first, like every
// other agent edit.
func (s *Server) suppressTombstoned(rel string) {
	if !index.IsMemoryPath(rel) {
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		return
	}
	body, n, err := seal.NewTombLog(s.Vault.Root).Strip(note.Body)
	if err != nil || n == 0 {
		return
	}
	s.History.Snapshot(rel, note.Body)
	if _, err := s.Vault.Write(rel, body, note.Frontmatter.Clone()); err != nil {
		return
	}
	_, _ = s.Index.Upsert(rel)
}

// suppressAllTombstoned applies the log to every memory note. Reindex runs it,
// because a reindex is what makes a note that arrived by copy visible.
func (s *Server) suppressAllTombstoned() {
	rows, err := s.Index.DB.Query("SELECT path FROM notes WHERE path LIKE 'memory/%'")
	if err != nil {
		return
	}
	var paths []string
	for rows.Next() {
		var p string
		if rows.Scan(&p) == nil {
			paths = append(paths, p)
		}
	}
	rows.Close()
	for _, p := range paths {
		s.suppressTombstoned(p)
	}
}
