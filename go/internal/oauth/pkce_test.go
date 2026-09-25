package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func challengeFor(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestVerifyPKCE(t *testing.T) {
	verifier := "a-very-random-verifier-value-that-is-long-enough-1234567890"
	challenge := challengeFor(verifier)

	if !VerifyPKCE(verifier, challenge, "S256") {
		t.Fatal("expected a matching S256 verifier/challenge pair to verify")
	}
	if VerifyPKCE("wrong-verifier", challenge, "S256") {
		t.Fatal("a mismatched verifier must not verify")
	}
	if VerifyPKCE(verifier, challenge, "plain") {
		t.Fatal("plain must never be accepted, even with a correct value")
	}
	if VerifyPKCE(verifier, challenge, "") {
		t.Fatal("an empty method must not verify")
	}
	if VerifyPKCE("", challenge, "S256") {
		t.Fatal("an empty verifier must not verify")
	}
	if VerifyPKCE(verifier, "", "S256") {
		t.Fatal("an empty challenge must not verify")
	}
}
