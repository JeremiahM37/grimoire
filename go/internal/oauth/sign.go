package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// authRequest is everything the consent page needs to remember between the
// GET that renders it and the POST that decides it.
type authRequest struct {
	ClientID            string   `json:"cid"`
	RedirectURI         string   `json:"ru"`
	State               string   `json:"st"`
	Requested           []string `json:"sc"`
	Resource            string   `json:"rs"`
	CodeChallenge       string   `json:"cc"`
	CodeChallengeMethod string   `json:"cm"`
	Expires             int64    `json:"exp"`
}

// signer HMAC-signs the consent form's hidden state field, so the POST
// handler can trust client_id, redirect_uri and the PKCE challenge without
// storing per-request server-side state, and so a cross-site POST cannot
// forge one — this doubles as the flow's CSRF token, for the reasons given
// in oauth.go's package doc.
//
// The key is generated fresh at process start and never persisted: a
// restart invalidates any consent page a browser still has open, which is a
// mildly annoying "start over" for the owner and not a problem for anyone
// else, since nothing of value existed yet at that point in the flow.
type signer struct {
	key [32]byte
}

func newSigner() (*signer, error) {
	var s signer
	if _, err := rand.Read(s.key[:]); err != nil {
		return nil, err
	}
	return &s, nil
}

const authRequestTTL = 10 * time.Minute

func (s *signer) sign(ar authRequest) (string, error) {
	ar.Expires = Now().Add(authRequestTTL).Unix()
	body, err := json.Marshal(ar)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(body)
	sig := mac.Sum(nil)
	payload := base64.RawURLEncoding.EncodeToString(body)
	sigEnc := base64.RawURLEncoding.EncodeToString(sig)
	return payload + "." + sigEnc, nil
}

var errBadSignature = errors.New("oauth: invalid or expired consent request")

func (s *signer) verify(token string) (authRequest, error) {
	var ar authRequest
	i := strings.IndexByte(token, '.')
	if i < 0 {
		return ar, errBadSignature
	}
	payload, sigEnc := token[:i], token[i+1:]
	body, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return ar, errBadSignature
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigEnc)
	if err != nil {
		return ar, errBadSignature
	}
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write(body)
	want := mac.Sum(nil)
	if subtle.ConstantTimeCompare(sig, want) != 1 {
		return ar, errBadSignature
	}
	if err := json.Unmarshal(body, &ar); err != nil {
		return ar, errBadSignature
	}
	if Now().Unix() > ar.Expires {
		return ar, errBadSignature
	}
	return ar, nil
}
