package oauth

import (
	"crypto/sha256"
	"encoding/base64"
)

// VerifyPKCE checks a code_verifier against the code_challenge recorded at
// /oauth/authorize time.
//
// Only S256 is accepted. OAuth 2.1 requires PKCE and requires S256 "when
// technically capable" (every client this server targets is), and the
// alternative method, "plain", sends the verifier itself as the challenge —
// accepting it would mean the value that protects the authorization code is
// the same value an eavesdropper on the redirect already saw. Refusing
// "plain" outright, rather than merely preferring S256, is what makes that
// refusal a guarantee instead of a client-side courtesy.
func VerifyPKCE(verifier, challenge, method string) bool {
	if verifier == "" || challenge == "" || method != "S256" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	computed := base64.RawURLEncoding.EncodeToString(sum[:])
	return computed == challenge
}
