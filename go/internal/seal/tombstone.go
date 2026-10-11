package seal

import (
	"bufio"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// TombstoneFile is the vault path of the tombstone log. It is a markdown file
// on purpose: the note sync and the peer manifest walk markdown only, so a
// log in any other format would never leave this device. Its lines are not
// bullets, so memory parsing ignores them.
const TombstoneFile = "memory-tombstones.md"

// macLabel domain-separates the tombstone MAC from every other use of the
// session key.
const macLabel = "grimoire/seal/tombstone-mac/v1"

// Tombstone records that one fact id was forgotten. The id itself is not
// stored, only a salted hash of it, so the log does not hand out the ids of
// everything a person has forgotten. The salt is per tombstone, so two logs
// cannot be matched against each other by hash.
//
// MAC authenticates the line under the vault session key. A line without one
// was written while the vault was locked, or before signing existed; it still
// suppresses, but is reported as unsigned. A line whose MAC does not verify is
// ignored: anyone who can write the file could otherwise suppress any fact.
type Tombstone struct {
	Salt   string `json:"salt"`
	Hash   string `json:"hash"`
	At     string `json:"at"`
	Reason string `json:"reason"`
	MAC    string `json:"mac,omitempty"`
}

// Status says how a tombstone was checked when the log was loaded.
type Status string

const (
	StatusVerified   Status = "verified"   // MAC checked under the session key
	StatusUnsigned   Status = "unsigned"   // no MAC: written locked, or before signing
	StatusUnverified Status = "unverified" // MAC present, but the vault is locked so it cannot be checked
	StatusRejected   Status = "rejected"   // MAC present and wrong: ignored
)

// Report counts what a load found. Rejected lines are never honoured.
type Report struct {
	Verified   int
	Unsigned   int
	Unverified int
	Rejected   int
}

// MACKey derives the tombstone MAC key from the vault session key. It is an
// HMAC of a fixed label under the session key, so the MAC key and the
// session key are never the same bytes and this use cannot be confused with
// any other.
func MACKey(sessionKey []byte) []byte {
	m := hmac.New(sha256.New, sessionKey)
	m.Write([]byte(macLabel))
	return m.Sum(nil)
}

// macOf computes a tombstone's MAC over every field it asserts.
func macOf(key []byte, t Tombstone) string {
	m := hmac.New(sha256.New, key)
	for _, f := range []string{t.Salt, t.Hash, t.At, t.Reason} {
		m.Write([]byte(f))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

// TombLog reads and appends the tombstone file under a vault root.
type TombLog struct {
	Path string
}

// NewTombLog returns the log for the vault at root.
func NewTombLog(root string) *TombLog {
	return &TombLog{Path: filepath.Join(root, TombstoneFile)}
}

// Hash is the salted hash a tombstone stores for a fact id.
func Hash(salt, id string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + id))
	return hex.EncodeToString(sum[:])
}

// Load reads the tombstones that suppress, given the session key. key is nil
// when the vault is locked. A locked load honours signed lines without
// checking them and marks them unverified, so a locked server still does not
// resurrect a forgotten fact, but the report says what was not checked. A
// missing file is an empty log. Lines that are not tombstones are skipped, so
// a person's own notes in the file survive.
func (t *TombLog) Load(key []byte) ([]Tombstone, Report, error) {
	var rep Report
	f, err := os.Open(t.Path)
	if os.IsNotExist(err) {
		return nil, rep, nil
	}
	if err != nil {
		return nil, rep, err
	}
	defer f.Close()
	var out []Tombstone
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ts Tombstone
		if err := json.Unmarshal([]byte(line), &ts); err != nil || ts.Hash == "" {
			continue
		}
		switch {
		case ts.MAC == "":
			rep.Unsigned++
		case key == nil:
			rep.Unverified++
		case hmac.Equal([]byte(ts.MAC), []byte(macOf(key, ts))):
			rep.Verified++
		default:
			rep.Rejected++
			continue
		}
		out = append(out, ts)
	}
	if err := sc.Err(); err != nil {
		return nil, rep, err
	}
	if rep.Rejected > 0 {
		log.Printf("seal: %d tombstone line(s) in %s failed verification and were ignored", rep.Rejected, TombstoneFile)
	}
	return out, rep, nil
}

// Add records that a fact id was forgotten. key is the session key, or nil
// when locked, in which case the line is written unsigned and is reported as
// such on every later load. Recording the same id twice is a no-op.
func (t *TombLog) Add(id, reason string, now time.Time, key []byte) error {
	ts, _, err := t.Load(key)
	if err != nil {
		return err
	}
	if Suppressed(ts, id) {
		return nil
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	rec := Tombstone{Salt: hex.EncodeToString(salt), At: now.Format(memory.StampFormat), Reason: reason}
	rec.Hash = Hash(rec.Salt, id)
	if key != nil {
		rec.MAC = macOf(key, rec)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	lines, err := t.lines()
	if err != nil {
		return err
	}
	return t.write(append(lines, string(line)))
}

// Resign moves signed tombstones from oldKey to newKey, for a passphrase
// change. Lines that do not verify under oldKey are left as they are, and
// unsigned lines stay unsigned. It returns how many lines it re-signed.
func (t *TombLog) Resign(oldKey, newKey []byte) (int, error) {
	lines, err := t.lines()
	if err != nil {
		return 0, err
	}
	n := 0
	for i, line := range lines {
		var ts Tombstone
		if !strings.HasPrefix(strings.TrimSpace(line), "{") || json.Unmarshal([]byte(line), &ts) != nil || ts.MAC == "" {
			continue
		}
		if !hmac.Equal([]byte(ts.MAC), []byte(macOf(oldKey, ts))) {
			continue
		}
		ts.MAC = macOf(newKey, ts)
		raw, err := json.Marshal(ts)
		if err != nil {
			return n, err
		}
		lines[i] = string(raw)
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, t.write(lines)
}

// Suppressed reports whether a fact id has a tombstone in ts.
func Suppressed(ts []Tombstone, id string) bool {
	if id == "" {
		return false
	}
	for _, t := range ts {
		if Hash(t.Salt, id) == t.Hash {
			return true
		}
	}
	return false
}

// StripBullets removes every bullet whose id is tombstoned. Every other line,
// including bullets that are not tombstoned, is kept byte for byte. The count
// says how many bullets went, so a caller can skip the rewrite when none did.
func StripBullets(body string, ts []Tombstone) (string, int) {
	if len(ts) == 0 {
		return body, 0
	}
	lines := strings.Split(body, "\n")
	out := lines[:0]
	n := 0
	for _, line := range lines {
		if e, ok := memory.ParseLine(line); ok && Suppressed(ts, e.ID) {
			n++
			continue
		}
		out = append(out, line)
	}
	if n == 0 {
		return body, 0
	}
	return strings.Join(out, "\n"), n
}

// Strip applies the log, loaded under key, to one note body.
func (t *TombLog) Strip(body string, key []byte) (string, int, error) {
	ts, _, err := t.Load(key)
	if err != nil {
		return body, 0, err
	}
	out, n := StripBullets(body, ts)
	return out, n, nil
}

// lines returns the file's current lines, or a heading when it is new.
func (t *TombLog) lines() ([]string, error) {
	raw, err := os.ReadFile(t.Path)
	if os.IsNotExist(err) || (err == nil && len(raw) == 0) {
		return []string{"# Memory tombstones", "", "Forgotten fact ids, salted and hashed. Written by grimoire; do not edit."}, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n"), nil
}

// write replaces the file atomically at 0600.
func (t *TombLog) write(lines []string) error {
	if err := os.MkdirAll(filepath.Dir(t.Path), 0o755); err != nil {
		return err
	}
	tmp := t.Path + ".tmp"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, t.Path)
}
