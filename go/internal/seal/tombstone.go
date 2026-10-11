package seal

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

// Tombstone records that one fact id was forgotten. The id itself is not
// stored, only a salted hash of it, so the log does not hand out the ids of
// everything a person has forgotten. The salt is per tombstone, so two logs
// cannot be matched against each other by hash.
type Tombstone struct {
	Salt   string `json:"salt"`
	Hash   string `json:"hash"`
	At     string `json:"at"`
	Reason string `json:"reason"`
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

// Load reads every tombstone. A missing file is an empty log. Lines that are
// not tombstones are skipped, so a person's own notes in the file survive.
func (t *TombLog) Load() ([]Tombstone, error) {
	f, err := os.Open(t.Path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
		out = append(out, ts)
	}
	return out, sc.Err()
}

// Add records that a fact id was forgotten. Recording the same id twice is a
// no-op: the first tombstone is the one that matters.
func (t *TombLog) Add(id, reason string, now time.Time) error {
	ts, err := t.Load()
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
	rec := Tombstone{Salt: hex.EncodeToString(salt), Hash: "", At: now.Format(memory.StampFormat), Reason: reason}
	rec.Hash = Hash(rec.Salt, id)
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return t.write(append(t.lines(), string(line)))
}

// Suppressed reports whether a fact id has a tombstone.
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

// Strip applies the log to one note body.
func (t *TombLog) Strip(body string) (string, int, error) {
	ts, err := t.Load()
	if err != nil {
		return body, 0, err
	}
	out, n := StripBullets(body, ts)
	return out, n, nil
}

// lines returns the file's current lines, or a heading when it is new.
func (t *TombLog) lines() []string {
	raw, err := os.ReadFile(t.Path)
	if err != nil || len(raw) == 0 {
		return []string{"# Memory tombstones", "", "Forgotten fact ids, salted and hashed. Written by grimoire; do not edit."}
	}
	return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
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
