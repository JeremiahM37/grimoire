package seal

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	keyID, token, err := ks.Seal(testMaster(t), "the deploy key lives in the vault")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(token, "deploy") {
		t.Fatal("ciphertext contains the plaintext")
	}
	got, err := ks.Open(testMaster(t), keyID, token)
	if err != nil || got != "the deploy key lives in the vault" {
		t.Fatalf("Open = %q, %v", got, err)
	}
}

func TestOpenNeedsTheRightSessionKey(t *testing.T) {
	t.Parallel()
	ks := NewKeystore(filepath.Join(t.TempDir(), "keys.json"))
	keyID, token, err := ks.Seal(testMaster(t), "secret")
	if err != nil {
		t.Fatal(err)
	}
	wrong := []byte("ZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZZ=")
	if _, err := ks.Open(wrong, keyID, token); err == nil {
		t.Fatal("a wrong session key opened the fact")
	}
}

func TestShredMakesEveryCopyUnreadableAndIsIdempotent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "keys.json")
	ks := NewKeystore(path)
	keyID, token, err := ks.Seal(testMaster(t), "gone soon")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatal(err)
	}
	if ks.Has(keyID) {
		t.Fatal("key still present after shred")
	}
	if _, err := ks.Open(testMaster(t), keyID, token); !errors.Is(err, ErrShredded) {
		t.Fatalf("Open after shred = %v, want ErrShredded", err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatalf("second shred = %v, want nil", err)
	}
	if err := ks.Shred(""); err != nil {
		t.Fatalf("shred of empty id = %v", err)
	}
	// Other keys survive a shred.
	otherID, otherTok, err := ks.Seal(testMaster(t), "kept")
	if err != nil {
		t.Fatal(err)
	}
	if err := ks.Shred(keyID); err != nil {
		t.Fatal(err)
	}
	if got, err := ks.Open(testMaster(t), otherID, otherTok); err != nil || got != "kept" {
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
	id1, tok1, _ := ks.Seal(testMaster(t), "same text")
	id2, tok2, _ := ks.Seal(testMaster(t), "same text")
	if id1 == id2 || tok1 == tok2 {
		t.Fatal("two seals of the same text shared a key or a ciphertext")
	}
}

func TestTombstoneHidesIDAndSuppresses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	log := NewTombLog(dir)
	now := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	if err := log.Add("abc123def456", "forget", now); err != nil {
		t.Fatal(err)
	}
	if err := log.Add("abc123def456", "forget", now); err != nil {
		t.Fatal(err)
	}
	ts, err := log.Load()
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
	if err := log.Add(gone.ID, "forget", time.Now()); err != nil {
		t.Fatal(err)
	}
	out, n, err := log.Strip(body)
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
	ts, err := NewTombLog(t.TempDir()).Load()
	if err != nil || len(ts) != 0 {
		t.Fatalf("Load on a missing log = %v, %v", ts, err)
	}
}
