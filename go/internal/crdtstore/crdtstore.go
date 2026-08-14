// Package crdtstore persists per-note CRDT documents for multi-device sync.
//
// Port of server/crdtstore.py. One JSON file per note under .grimoire/crdt/,
// named by a hash of the note path so nested paths and unicode names cannot
// produce an unsafe filename.
package crdtstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/crdt"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// MaxCRDTBytes bounds what will be tracked. A CRDT stores one atom per
// character, so a huge note costs far more in replica state than sync is worth;
// past this size the note simply is not mergeable.
const MaxCRDTBytes = 200_000

// Store owns the CRDT directory and this replica's site id.
type Store struct {
	Dir    string
	idPath string
	siteID string
}

func New(grimoireDir string) *Store {
	return &Store{
		Dir:    filepath.Join(grimoireDir, "crdt"),
		idPath: filepath.Join(grimoireDir, "site_id"),
	}
}

// SiteID identifies this replica. It is generated once and persisted: a replica
// that changed identity would re-issue atom ids it had already used, and
// convergence depends on those ids being unique per site.
func (s *Store) SiteID() string {
	if s.siteID != "" {
		return s.siteID
	}
	if raw, err := os.ReadFile(s.idPath); err == nil {
		if id := strings.TrimSpace(string(raw)); id != "" {
			s.siteID = id
			return id
		}
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		s.siteID = "local"
		return s.siteID
	}
	s.siteID = hex.EncodeToString(buf)
	_ = os.MkdirAll(filepath.Dir(s.idPath), 0o755)
	_ = os.WriteFile(s.idPath, []byte(s.siteID), 0o644)
	return s.siteID
}

func (s *Store) docFile(rel string) string {
	sum := sha256.Sum256([]byte(rel))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])[:16]+".json")
}

// Mergeable reports whether a note participates in CRDT sync. Encrypted bodies
// are excluded deliberately: merging ciphertext character by character would
// produce garbage, and decrypting in order to merge would defeat at-rest
// encryption.
func Mergeable(rel, body string) bool {
	return !vault.IsReserved(rel) &&
		!vault.IsEncrypted(body) &&
		len(body) <= MaxCRDTBytes
}

// LoadDoc returns the stored document, or nil when absent or unreadable.
func (s *Store) LoadDoc(rel string) *crdt.Doc {
	raw, err := os.ReadFile(s.docFile(rel))
	if err != nil {
		return nil
	}
	doc, err := crdt.FromJSON(string(raw), s.SiteID())
	if err != nil {
		return nil // a corrupt doc is rebuilt from the body, never fatal
	}
	return doc
}

// SaveDoc persists a document.
func (s *Store) SaveDoc(rel string, doc *crdt.Doc) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	p := s.docFile(rel)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(doc.ToJSON()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// DeleteDoc drops a note's replica state.
func (s *Store) DeleteDoc(rel string) error {
	if err := os.Remove(s.docFile(rel)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// reconciled returns the document brought up to date with the current body.
func (s *Store) reconciled(rel, body string) *crdt.Doc {
	doc := s.LoadDoc(rel)
	if doc == nil {
		return crdt.FromText(body, s.SiteID())
	}
	doc.Site = s.SiteID()
	doc.LocalEdit(body)
	return doc
}

// UpdateFromBody reconciles the current note body into its CRDT document.
func (s *Store) UpdateFromBody(rel, body string) error {
	if !Mergeable(rel, body) {
		return nil
	}
	return s.SaveDoc(rel, s.reconciled(rel, body))
}

// BodyDocJSON returns the serialized document, reconciled with the body first
// so a peer never receives state that is behind the file on disk.
func (s *Store) BodyDocJSON(rel, body string) (string, error) {
	doc := s.reconciled(rel, body)
	if err := s.SaveDoc(rel, doc); err != nil {
		return "", err
	}
	return doc.ToJSON(), nil
}

// Merge folds a peer's document into ours and returns the converged text.
func (s *Store) Merge(rel, body, peerJSON string) (string, error) {
	peer, err := crdt.FromJSON(peerJSON, "peer")
	if err != nil {
		return "", err
	}
	mine := s.reconciled(rel, body)
	mine.Merge(peer)
	if err := s.SaveDoc(rel, mine); err != nil {
		return "", err
	}
	return mine.Text(), nil
}
