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
	text := strings.TrimSpace(string(raw))

	// A secrets file grows. This one was a single `passphrase:` line until
	// something else worth keeping beside it was added — an admin token, in
	// the deployment that found this — and a reader that accepts only one line
	// then refuses to unlock the vault at the next restart, with an error
	// about line counts rather than about what changed.
	//
	// So a `passphrase:` key is taken from a flat key/value file whatever else
	// is in it. A file with no such key is treated as the passphrase itself,
	// which is the other shape people use, and only THAT case has to be a
	// single line — because there is no way to tell which line was meant.
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "passphrase:"); ok {
			if phrase := trimQuotes(strings.TrimSpace(rest)); phrase != "" {
				return phrase, nil
			}
		}
	}
	if text == "" {
		return "", fmt.Errorf("%s contains no passphrase", path)
	}
	if strings.Contains(text, "\n") {
		return "", fmt.Errorf("%s has several lines and none of them is a "+
			"passphrase: key — name it, or leave the file as the passphrase alone", path)
	}
	return text, nil
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
