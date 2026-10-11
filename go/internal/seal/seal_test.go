package seal

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// testMaster is a fixed session key. The real one is derived from the vault
// passphrase; the seal layer only ever sees it as opaque key material.
func testMaster(t *testing.T) []byte {
	t.Helper()
	return []byte("aOwAyK0Cj1q8Bv2a3J5Hq9sZk4mVtXrN7pQeYdLh0vE=")
}

func TestSealOpenRoundTripAndNoPlaintextInToken(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	keyID, token, err := ks.Seal(testMaster(t), "alice", "the deploy key lives in the vault")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(token, "deploy") {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := ks.Open(testMaster(t), "alice", keyID, token)
	if err != nil || got != "the deploy key lives in the vault" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenNeedsTheRightSessionKey(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	keyID, token, err := ks.Seal(testMaster(t), "alice", "secret")
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ=")
	if _, err := ks.Open(wrong, "alice", keyID, token); err == nil {
		t.Fatal("a wrong session key opened the fact")
	}
}

func TestShredMakesEveryCopyUnreadableAndIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	ks := NewKeystore(path)
	keyID, token, err := ks.Seal(testMaster(t), "alice", "gone soon")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatal(err)
	}
	if ks.Has(keyID) {
		t.Fatal("key still present after shred")
	}
	if _, err := ks.Open(testMaster(t), "alice", keyID, token); !errors.Is(err, ErrShredded) {
		t.Fatalf("Open after shred = %v, want ErrShredded", err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatalf("second shred = %v, want nil", err)
	}
	if err := ks.Shred(""); err != nil {
		t.Fatalf("shred of empty id = %v", err)
	}
	// Other keys survive a shred.
	otherID, otherTok, err := ks.Seal(testMaster(t), "alice", "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Open(testMaster(t), "alice", otherID, otherTok); err != nil || got != "kept" {
		t.Fatalf("unrelated key lost in shred: %q, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key store mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
}

func TestEachSealUsesAFreshKey(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	id1, tok1, _ := ks.Seal(testMaster(t), "alice", "same text")
	id2, tok2, _ := ks.Seal(testMaster(t), "alice", "same text")
	if id1 == id2 || tok1 == tok2 {
		t.Fatal("two seals of the same text shared a key or a ciphertext")
	}
}

func TestTombstoneHidesIDAndSuppresses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewTombLog(dir)
	key := MACKey(testMaster(t))
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	if err := log.Add("abc123def456", "forget", now, key); err != nil {
		t.Fatal(err)
	}
	if err := log.Add("abc123def456", "forget", now, key); err != nil {
		t.Fatal(err)
	}
	ts, _, err := log.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("repeated Add wrote %d tombstones, want 1", len(ts))
	}
	raw, _ := os.ReadFile(log.Path)
	if bytes.Contains(raw, []byte("abc123def456")) {
		t.Fatal("the log stores the fact id in clear")
	}
	if !Suppressed(ts, "abc123def456") || Suppressed(ts, "fff000fff000") {
		t.Fatal("Suppressed does not match exactly the forgotten id")
	}
}

func TestStripBulletsRemovesOnlyTombstonedBullets(t *testing.T) {
	t.Parallel()
	gone := memory.Entry{ID: memory.DeriveID("2026-10-10 09:00", "agent", "gone fact"),
		Text: "gone fact", Agent: "agent", Stamp: "2026-10-10 09:00"}
	kept := memory.Entry{ID: memory.DeriveID("2026-10-10 09:01", "agent", "kept fact"),
		Text: "kept fact", Agent: "agent", Stamp: "2026-10-10 09:01"}
	body := "# Memory\n\nnot a bullet, a person typed this\n" + gone.Format() + "\n" + kept.Format() + "\n"

	log := NewTombLog(t.TempDir())
	key := MACKey(testMaster(t))
	if err := log.Add(gone.ID, "forget", time.Now(), key); err != nil {
		t.Fatal(err)
	}
	out, n, err := log.Strip(body, key)
	if err != nil || n != 1 {
		t.Fatalf("Strip removed %d (%v), want 1", n, err)
	}
	want := "# Memory\n\nnot a bullet, a person typed this\n" + kept.Format() + "\n"
	if out != want {
		t.Fatalf("Strip changed more than the tombstoned line:\n%q\nwant\n%q", out, want)
	}
	if out2, n2 := StripBullets(out, nil); n2 != 0 || out2 != out {
		t.Fatal("an empty log must change nothing")
	}
}

func TestTombLogMissingFileIsEmpty(t *testing.T) {
	t.Parallel()
	ts, _, err := NewTombLog(t.TempDir()).Load(nil)
	if err != nil || len(ts) != 0 {
		t.Fatalf("Load on a missing log = %v, %v", ts, err)
	}
}

// The tombstone MAC: a signed line verifies under the session key it was
// written with, a forged or re-keyed line is ignored, an unsigned line still
// suppresses but is reported, and a locked load cannot verify and says so.
func TestTombstoneMACVerifiesRejectsAndReports(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewTombLog(dir)
	key := MACKey(testMaster(t))
	now := time.Now()
	if err := log.Add("signed-id-0001", "forget", now, key); err != nil {
		t.Fatal(err)
	}
	if err := log.Add("unsigned-id-02", "forget", now, nil); err != nil {
		t.Fatal(err)
	}
	ts, rep, err := log.Load(key)
	if err != nil || rep.Verified != 1 || rep.Unsigned != 1 || rep.Rejected != 0 || len(ts) != 1 {
		t.Fatalf("unlocked load = %d honoured, %+v, %v; want only the verified line", len(ts), rep, err)
	}
	if Suppressed(ts, "unsigned-id-02") {
		t.Fatal("an unsigned line suppressed a fact")
	}
	ts, rep, _ = log.Load(nil)
	if rep.Unverified != 1 || rep.Unsigned != 1 || len(ts) != 0 {
		t.Fatalf("a locked load verifies nothing, so it honours nothing; got %d honoured, %+v", len(ts), rep)
	}

	// A line edited to name another id, with its MAC left as it was, fails.
	raw, _ := os.ReadFile(log.Path)
	forged := strings.Replace(string(raw), Hash(findSalt(t, log, "signed-id-0001", key), "signed-id-0001"),
		Hash(findSalt(t, log, "signed-id-0001", key), "victim-id-00003"), 1)
	if forged == string(raw) {
		t.Fatal("test did not forge anything")
	}
	os.WriteFile(log.Path, []byte(forged), 0o600)
	ts, rep, _ = log.Load(key)
	if rep.Rejected != 1 {
		t.Fatalf("forged line not rejected: %+v", rep)
	}
	if Suppressed(ts, "victim-id-00003") {
		t.Fatal("a forged tombstone suppressed a fact")
	}
	// A line signed under another session key is not honoured either.
	other := MACKey([]byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ="))
	if _, rep, _ := log.Load(other); rep.Rejected == 0 {
		t.Fatal("a line signed under another key verified")
	}
}

// findSalt returns the salt of the signed tombstone for id, so a test can
// forge a line against the real hash.
func findSalt(t *testing.T, log *TombLog, id string, key []byte) string {
	t.Helper()
	ts, _, err := log.Load(key)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ts {
		if Hash(x.Salt, id) == x.Hash {
			return x.Salt
		}
	}
	t.Fatal("no tombstone for " + id)
	return ""
}

// Resign moves signed lines to a new session key after a passphrase change,
// leaves unsigned lines alone, and the moved lines verify under the new key.
func TestResignFollowsThePassphrase(t *testing.T) {
	t.Parallel()
	log := NewTombLog(t.TempDir())
	oldKey := MACKey(testMaster(t))
	newKey := MACKey([]byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ="))
	if err := log.Add("resign-id-00001", "forget", time.Now(), oldKey); err != nil {
		t.Fatal(err)
	}
	if err := log.Add("resign-id-00002", "forget", time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	n, err := log.Resign(oldKey, newKey)
	if err != nil || n != 1 {
		t.Fatalf("Resign = %d, %v; want 1", n, err)
	}
	ts, rep, _ := log.Load(newKey)
	if rep.Verified != 1 || rep.Unsigned != 1 || rep.Rejected != 0 || len(ts) != 1 {
		t.Fatalf("after resign: %+v", rep)
	}
}

// Keys wrapped under the old session key open under the new one after Rewrap,
// and a key that does not unwrap under the old key blocks the whole rewrite.
func TestKeystoreRewrapFollowsThePassphrase(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	oldM := testMaster(t)
	newM := []byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ=")
	id, tok, err := ks.Seal(oldM, "alice", "rewrapped")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Rewrap(oldM, newM); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Open(newM, "alice", id, tok); err != nil || got != "rewrapped" {
		t.Fatalf("after rewrap: %q, %v", got, err)
	}
	if _, err := ks.Open(oldM, "alice", id, tok); err == nil {
		t.Fatal("the old session key still opens a rewrapped key")
	}
}

// A line anyone could append (a hash of a visible fact id, no MAC) must not
// delete the fact, in either an unlocked or a locked load.
func TestForgedUnsignedTombstoneDeletesNothing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewTombLog(dir)
	key := MACKey(testMaster(t))
	victim := memory.Entry{ID: memory.DeriveID("2026-10-10 09:00", "alice", "alice fact"),
		Text: "alice fact", Agent: "alice", Stamp: "2026-10-10 09:00"}
	body := victim.Format() + "\n"

	forged := Tombstone{Salt: "x", At: "2026-10-10 09:00", Reason: "forget"}
	forged.Hash = Hash(forged.Salt, victim.ID)
	raw, _ := json.Marshal(forged)
	if err := os.WriteFile(log.Path, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, k := range [][]byte{key, nil} {
		out, n, err := log.Strip(body, k)
		if err != nil || n != 0 || out != body {
			t.Fatalf("a forged unsigned tombstone removed %d bullets (key set: %v)", n, k != nil)
		}
	}
}

// Another account's copy of a sealed bullet opens nothing, because the owner
// is recorded in the key store and not in the note.
func TestSealedKeyServesOnlyItsOwner(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	id, tok, err := ks.Seal(testMaster(t), "user:alice", "alice's secret")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Open(testMaster(t), "user:alice", id, tok); err != nil || got != "alice's secret" {
		t.Fatalf("owner open = %q, %v", got, err)
	}
	for _, other := range []string{"user:bob", "local", ""} {
		if got, err := ks.Open(testMaster(t), other, id, tok); !errors.Is(err, ErrShredded) || got != "" {
			t.Fatalf("owner %q got %q, %v; want ErrShredded", other, got, err)
		}
	}
}

// Concurrent seals and shreds on separate Keystore values for one path never
// lose a write or bring a shredded key back.
func TestKeystoreConcurrentSealAndShred(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	master := testMaster(t)
	first, _, err := NewKeystore(path).Seal(master, "o", "to shred")
	if err != nil {
		t.Fatal(err)
	}
	const n = 16
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, _, err := NewKeystore(path).Seal(master, "o", "fact")
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := NewKeystore(path).Shred(first); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	ks := NewKeystore(path)
	if ks.Has(first) {
		t.Fatal("a shredded key came back")
	}
	for i, id := range ids {
		if id == "" || !ks.Has(id) {
			t.Fatalf("seal %d lost its key", i)
		}
	}
}
