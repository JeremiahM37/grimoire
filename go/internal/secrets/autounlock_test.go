package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func passphraseFile(t *testing.T, contents string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vault.yaml")
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestReadPassphraseFileShapes(t *testing.T) {
	const want = "correct horse battery staple"
	for name, contents := range map[string]string{
		"bare":          want,
		"trailing new":  want + "\n",
		"yaml":          "passphrase: " + want + "\n",
		"yaml quoted":   "passphrase: \"" + want + "\"\n",
		"yaml single":   "passphrase: '" + want + "'\n",
		"yaml no space": "passphrase:" + want,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadPassphraseFile(passphraseFile(t, contents, 0o600))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}
}

// The file is the whole security of an unattended unlock: if anyone else on the
// box can read it, the vault's passphrase is theirs.
func TestReadPassphraseFileRefusesLoosePermissions(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o604, 0o666} {
		if _, err := ReadPassphraseFile(passphraseFile(t, "a passphrase", mode)); err == nil {
			t.Fatalf("mode %#o accepted", mode)
		}
	}
}

func TestReadPassphraseFileRejectsEmptyAndMultiline(t *testing.T) {
	if _, err := ReadPassphraseFile(passphraseFile(t, "  \n", 0o600)); err == nil {
		t.Fatal("empty file accepted")
	}
	if _, err := ReadPassphraseFile(passphraseFile(t, "one\ntwo\n", 0o600)); err == nil {
		t.Fatal("multi-line file accepted")
	}
	if _, err := ReadPassphraseFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestUnlockFromFile(t *testing.T) {
	v, _ := testVault(t)
	path := passphraseFile(t, "passphrase: correct horse battery staple\n", 0o600)

	// A vault that does not exist yet is reported as such, not initialized
	// behind the operator's back.
	if err := v.UnlockFromFile(path); !errors.Is(err, ErrNotInit) {
		t.Fatalf("uninitialized vault: got %v, want ErrNotInit", err)
	}
	if v.IsInitialized() {
		t.Fatal("auto-unlock initialized a vault on its own")
	}

	if err := v.Initialize("correct horse battery staple"); err != nil {
		t.Fatal(err)
	}
	v.Lock()
	if err := v.UnlockFromFile(path); err != nil {
		t.Fatal(err)
	}
	if !v.IsUnlocked() {
		t.Fatal("vault still locked after unlocking from file")
	}
	// Idempotent: a second call on an unlocked vault is a no-op, not a second
	// key derivation or a failure counted against the lockout.
	if err := v.UnlockFromFile(path); err != nil {
		t.Fatal(err)
	}

	v.Lock()
	wrong := passphraseFile(t, "passphrase: not the passphrase\n", 0o600)
	if err := v.UnlockFromFile(wrong); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: got %v, want ErrPassphrase", err)
	}
	if v.IsUnlocked() {
		t.Fatal("vault unlocked with the wrong passphrase")
	}
}

// A secrets file grows. This one held a single passphrase line until an admin
// token was added beside it, at which point the vault stopped unlocking at the
// next restart — with an error about line counts rather than about what
// changed. A named key must be found whatever else the file holds.
func TestPassphraseIsFoundBesideOtherKeys(t *testing.T) {
	const want = "correct horse battery staple"
	for name, contents := range map[string]string{
		"passphrase first": "passphrase: " + want + "\nadmin_token: abc123\n",
		"passphrase last":  "admin_token: abc123\npassphrase: " + want + "\n",
		"with comments":    "# the vault\npassphrase: " + want + "\n# and more\nother: x\n",
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadPassphraseFile(passphraseFile(t, contents, 0o600))
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		})
	}

	// Without a named key, several lines are ambiguous — there is no way to
	// tell which one was meant — and that stays an error.
	_, err := ReadPassphraseFile(passphraseFile(t, "one\ntwo\n", 0o600))
	if err == nil {
		t.Fatal("an unnamed multi-line file was accepted")
	}
	if !strings.Contains(err.Error(), "passphrase:") {
		t.Fatalf("the error does not say how to fix it: %v", err)
	}
}
