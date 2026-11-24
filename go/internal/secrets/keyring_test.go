package secrets

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeKeyring stands in for secret-tool / security and records what reached
// them, so a test can check the passphrase never went on a command line.
type fakeKeyring struct {
	store map[string]string
	argv  []string
}

func installFakeKeyring(t *testing.T, osName string) *fakeKeyring {
	f := &fakeKeyring{store: map[string]string{}}
	oldRun, oldLook, oldOS := keyringRun, keyringLookPath, keyringOS
	t.Cleanup(func() { keyringRun, keyringLookPath, keyringOS = oldRun, oldLook, oldOS })
	keyringOS = osName
	keyringLookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	keyringRun = func(name, stdin string, args ...string) (string, error) {
		f.argv = append(f.argv, strings.Join(args, " "))
		switch {
		case len(args) > 0 && args[0] == "store":
			f.store[args[len(args)-1]] = stdin
		case len(args) > 0 && args[0] == "lookup":
			if v, ok := f.store[args[len(args)-1]]; ok {
				return v, nil
			}
			return "", os.ErrNotExist
		case len(args) > 0 && args[0] == "clear":
			delete(f.store, args[len(args)-1])
		case len(args) > 0 && args[0] == "-i":
			fields := strings.Fields(stdin)
			raw, _ := hex.DecodeString(fields[len(fields)-1])
			f.store[fields[5]] = string(raw)
		case len(args) > 0 && args[0] == "delete-generic-password":
			delete(f.store, args[4])
		case len(args) > 0 && args[0] == "find-generic-password":
			if v, ok := f.store[args[4]]; ok {
				return v + "\n", nil
			}
			return "", os.ErrNotExist
		}
		return "", nil
	}
	return f
}

func TestKeyringStoresOnStdinAndUnlocks(t *testing.T) {
	for _, osName := range []string{"linux", "darwin"} {
		t.Run(osName, func(t *testing.T) {
			f := installFakeKeyring(t, osName)
			t.Setenv("GRIMOIRE_VAULT_PASSPHRASE_FILE", "")
			t.Setenv("CREDENTIALS_DIRECTORY", "")
			dir := t.TempDir()
			v := New(dir)
			if err := v.Initialize("correct horse battery"); err != nil {
				t.Fatal(err)
			}
			if err := v.StoreKeyringPassphrase("wrong passphrase!"); err == nil {
				t.Fatal("a passphrase that does not open the vault must not be stored")
			}
			if err := v.StoreKeyringPassphrase("correct horse battery"); err != nil {
				t.Fatal(err)
			}
			for _, a := range f.argv {
				if strings.Contains(a, "correct horse") {
					t.Fatalf("passphrase on a command line: %q", a)
				}
			}
			fresh := New(dir)
			source, err := fresh.AutoUnlock()
			if err != nil || source != SourceKeyring || !fresh.IsUnlocked() {
				t.Fatalf("%v %v", source, err)
			}
			if err := fresh.ForgetKeyringPassphrase(); err != nil {
				t.Fatal(err)
			}
			if _, err := New(dir).AutoUnlock(); err != ErrNotInKeyring {
				t.Fatalf("after forget: %v", err)
			}
		})
	}
}

func TestSystemdCredentialComesFirst(t *testing.T) {
	installFakeKeyring(t, "linux")
	dir := t.TempDir()
	v := New(dir)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	creds := t.TempDir()
	if err := os.WriteFile(filepath.Join(creds, SystemdCredential), []byte("correct horse battery\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", creds)
	t.Setenv("GRIMOIRE_VAULT_PASSPHRASE_FILE", "")
	source, err := New(dir).AutoUnlock()
	if err != nil || source != SourceSystemd {
		t.Fatalf("%v %v", source, err)
	}
}
