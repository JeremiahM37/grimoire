package secrets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Unlocking from the operating system's own secret store.
//
// GRIMOIRE_VAULT_PASSPHRASE_FILE works, but it is the passphrase in a file:
// anything that can read the service user's home can open every credential.
// The operating system already has a place for a secret that should open when
// its owner logs in, so the passphrase can live there instead:
//
//   - macOS: the login Keychain, unlocked when the user logs in;
//   - Linux desktops: the Secret Service (GNOME Keyring, KWallet), which PAM
//     unlocks at login, reached with secret-tool;
//   - Linux services: a systemd credential (systemd-creds encrypt, then
//     LoadCredentialEncrypted= in the unit). It is sealed to the machine's
//     TPM or host key, decrypted only into the service's private credentials
//     directory, and never sits on disk in the clear.
//
// `grimoire secret keyring on` stores the passphrase in the login keyring;
// the server tries the systemd credential, then the keyring, then the
// passphrase file, and keeps trying the keyring for a while if the login
// session has not unlocked it yet.

// KeyringService is the service name the passphrase is stored under.
const KeyringService = "grimoire-vault"

// SystemdCredential is the credential name LoadCredentialEncrypted= must use.
const SystemdCredential = "grimoire-vault"

// ErrNoKeyring means this machine has no keyring Grimoire can use.
var ErrNoKeyring = errors.New("no OS keyring available (macOS Keychain, or secret-tool for the Secret Service on Linux)")

// ErrNotInKeyring means the keyring works but holds no passphrase for this vault.
var ErrNotInKeyring = errors.New("the OS keyring holds no passphrase for this vault")

// keyringRun runs a keyring tool with stdin; replaced in tests.
var keyringRun = func(name string, stdin string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errOut.String()); msg != "" {
			return "", fmt.Errorf("%s: %s", name, msg)
		}
		return "", err
	}
	return out.String(), nil
}

// keyringLookPath finds a keyring tool; replaced in tests.
var keyringLookPath = exec.LookPath

var keyringOS = runtime.GOOS

// keyringAccount names this vault in the keyring. A machine can hold more
// than one vault, so the account is derived from the vault's path.
func (v *Vault) keyringAccount() string {
	abs, err := filepath.Abs(v.Path)
	if err != nil {
		abs = v.Path
	}
	sum := sha256.Sum256([]byte(abs))
	return "vault-" + hex.EncodeToString(sum[:6])
}

func keyringTool() (string, error) {
	name := "secret-tool"
	if keyringOS == "darwin" {
		name = "security"
	} else if keyringOS != "linux" && keyringOS != "freebsd" {
		return "", ErrNoKeyring
	}
	path, err := keyringLookPath(name)
	if err != nil {
		return "", ErrNoKeyring
	}
	return path, nil
}

// KeyringPassphrase reads this vault's passphrase from the OS keyring.
func (v *Vault) KeyringPassphrase() (string, error) {
	tool, err := keyringTool()
	if err != nil {
		return "", err
	}
	account := v.keyringAccount()
	var out string
	if keyringOS == "darwin" {
		out, err = keyringRun(tool, "", "find-generic-password", "-s", KeyringService, "-a", account, "-w")
	} else {
		out, err = keyringRun(tool, "", "lookup", "service", KeyringService, "account", account)
	}
	phrase := strings.TrimRight(out, "\r\n")
	if err != nil || phrase == "" {
		// Both tools exit non-zero for "no such item" and for a locked or
		// absent keyring alike; neither can be told apart reliably here.
		return "", ErrNotInKeyring
	}
	return phrase, nil
}

// StoreKeyringPassphrase saves the passphrase in the OS keyring after
// checking it opens this vault. The value goes to the tool on stdin, never
// on its command line, where any user could read it from the process list.
func (v *Vault) StoreKeyringPassphrase(phrase string) error {
	if err := v.Unlock(phrase); err != nil {
		return err
	}
	tool, err := keyringTool()
	if err != nil {
		return err
	}
	account := v.keyringAccount()
	if keyringOS == "darwin" {
		// security -i reads commands from stdin; -X takes the password as hex,
		// so no quoting can go wrong.
		line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -X %s\n",
			KeyringService, account, "Grimoire-vault", hex.EncodeToString([]byte(phrase)))
		_, err = keyringRun(tool, line, "-i")
		return err
	}
	_, err = keyringRun(tool, phrase, "store", "--label=Grimoire vault ("+filepath.Dir(v.Path)+")",
		"service", KeyringService, "account", account)
	return err
}

// ForgetKeyringPassphrase removes the passphrase from the OS keyring.
func (v *Vault) ForgetKeyringPassphrase() error {
	tool, err := keyringTool()
	if err != nil {
		return err
	}
	account := v.keyringAccount()
	if keyringOS == "darwin" {
		_, err = keyringRun(tool, "", "delete-generic-password", "-s", KeyringService, "-a", account)
	} else {
		_, err = keyringRun(tool, "", "clear", "service", KeyringService, "account", account)
	}
	return err
}

// SystemdCredentialPath is where systemd put the decrypted credential for
// this service, or "" when the unit loads none.
func SystemdCredentialPath() string {
	dir := os.Getenv("CREDENTIALS_DIRECTORY")
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, SystemdCredential)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// UnlockSource names where an automatic unlock found the passphrase.
type UnlockSource string

const (
	SourceSystemd UnlockSource = "systemd credential"
	SourceKeyring UnlockSource = "OS keyring"
	SourceFile    UnlockSource = "passphrase file"
)

// AutoUnlock opens an initialized vault without a person: from a systemd
// credential, then the OS keyring (unless GRIMOIRE_VAULT_KEYRING=0), then
// GRIMOIRE_VAULT_PASSPHRASE_FILE. It reports which source worked. With none
// configured it returns ErrNotInKeyring.
func (v *Vault) AutoUnlock() (UnlockSource, error) {
	if !v.IsInitialized() {
		return "", ErrNotInit
	}
	if v.IsUnlocked() {
		return "", nil
	}
	var errs []error
	if p := SystemdCredentialPath(); p != "" {
		// Not ReadPassphraseFile: systemd hands credentials over as 0440, and
		// what keeps them private is the directory, which only this service
		// can enter, so the file's group bits mean nothing here.
		raw, err := os.ReadFile(p)
		phrase := strings.TrimRight(string(raw), "\r\n")
		if err == nil && phrase == "" {
			err = errors.New("empty")
		}
		if err == nil {
			if err = v.Unlock(phrase); err == nil {
				return SourceSystemd, nil
			}
		}
		errs = append(errs, fmt.Errorf("systemd credential: %w", err))
	}
	if os.Getenv("GRIMOIRE_VAULT_KEYRING") != "0" {
		phrase, err := v.KeyringPassphrase()
		if err == nil {
			if err = v.Unlock(phrase); err == nil {
				return SourceKeyring, nil
			}
			errs = append(errs, fmt.Errorf("OS keyring: %w", err))
		} else if !errors.Is(err, ErrNoKeyring) && !errors.Is(err, ErrNotInKeyring) {
			errs = append(errs, fmt.Errorf("OS keyring: %w", err))
		}
	}
	if p := strings.TrimSpace(os.Getenv("GRIMOIRE_VAULT_PASSPHRASE_FILE")); p != "" {
		err := v.UnlockFromFile(p)
		if err == nil {
			return SourceFile, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", p, err))
	}
	if len(errs) == 0 {
		return "", ErrNotInKeyring
	}
	return "", errors.Join(errs...)
}
