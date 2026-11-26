package secrets

import "strings"

// credentialWords mark a key whose value is a credential.
var credentialWords = []string{"token", "key", "secret", "pass", "pwd", "auth", "credential",
	"private", "cookie", "session", "jwt", "bearer", "cert", "signature", "salt", "otp", "pin", "account_number"}

// configSuffixes mark a key that names or locates a credential rather than
// being one: proxmox.token_name, tailscale.client_id, unifi.base_url.
var configSuffixes = []string{"_name", "_user", "_id", "_url", "_host", "username", "email"}

// CredentialKey reports whether a dotted key names a credential, judged by its
// last part: unifi.api_key is one, unifi.base_url is not.
func CredentialKey(k string) bool {
	last := strings.ToLower(k[strings.LastIndex(k, ".")+1:])
	for _, sfx := range configSuffixes {
		if strings.HasSuffix(last, sfx) {
			return false
		}
	}
	for _, w := range credentialWords {
		if strings.Contains(last, w) {
			return true
		}
	}
	return false
}
