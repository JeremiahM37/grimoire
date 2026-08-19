package secrets

import (
	"fmt"
	"os"
	"strings"
)

// Unattended unlock, for a server whose agents need the broker without a human
// at the console.
//
// The vault's normal contract is that the key exists only after a person types
// the passphrase. A headless deployment cannot honour that: every restart —
// including an automatic one after a crash — leaves the broker locked, and
// every call an agent makes to use_credential fails with 423 until someone
// notices. Deployments therefore end up bolting on a unit that POSTs the
// passphrase to /api/vault/unlock at boot, which is strictly worse: the
// passphrase travels over the HTTP surface, and boot is the only moment it
// happens, so a later restart silently disarms the broker.
//
// GRIMOIRE_VAULT_PASSPHRASE_FILE makes that an explicit, opt-in feature of the
// server instead. The trade-off is stated plainly: a passphrase readable by
// the service user is a passphrase available to anything running as that user,
// so this trades the "only a human can unlock" property for availability. It
// is off unless the variable is set, and the file must not be readable by
// group or other.

// ReadPassphraseFile loads an unattended-unlock passphrase.
//
// It accepts either a file whose entire contents are the passphrase, or a
// single `passphrase: value` line, so an existing YAML one-liner from a
// secrets directory can be pointed at directly rather than copied into a
// second file (copies of a passphrase are the thing to avoid here).
func ReadPassphraseFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if mode := info.Mode().Perm(); mode&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by group or other (mode %#o)", path, mode)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	phrase := strings.TrimSpace(string(raw))
	// A YAML one-liner is the shape these files already have in the wild.
	if rest, ok := strings.CutPrefix(phrase, "passphrase:"); ok && !strings.Contains(rest, "\n") {
		phrase = strings.TrimSpace(rest)
		phrase = trimQuotes(phrase)
	}
	if phrase == "" {
		return "", fmt.Errorf("%s contains no passphrase", path)
	}
	if strings.Contains(phrase, "\n") {
		return "", fmt.Errorf("%s contains more than one line", path)
	}
	return phrase, nil
}

func trimQuotes(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// UnlockFromFile unlocks an initialized vault with a passphrase read from disk.
//
// It never initializes: creating a vault is a deliberate act, and doing it
// implicitly from a file that happened to exist would seal an empty store
// under a passphrase nobody chose.
func (v *Vault) UnlockFromFile(path string) error {
	if !v.IsInitialized() {
		return ErrNotInit
	}
	if v.IsUnlocked() {
		return nil
	}
	phrase, err := ReadPassphraseFile(path)
	if err != nil {
		return err
	}
	return v.Unlock(phrase)
}
