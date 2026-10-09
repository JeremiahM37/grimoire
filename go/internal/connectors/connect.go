package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Connecting an account.
//
// Google and Microsoft both support the OAuth "installed application" flow:
// open the consent page in a browser, receive the answer on a loopback port,
// exchange the code (with PKCE) for tokens. The only thing Grimoire cannot do
// is register the app for you — that is a human step in the Google Cloud /
// Azure portal, walked through in docs/CONNECTORS.md. GitHub has a device flow
// that needs no redirect. Slack requires an https redirect URL, which a
// loopback listener is not, so Slack is a token paste.

// LoopbackOptions configures the browser flow.
type LoopbackOptions struct {
	Provider     string // google | microsoft
	ClientID     string
	ClientSecret string // Google desktop clients have one; Microsoft public clients do not
	Tenant       string // Microsoft; default "common"
	Scopes       []string
	// Open shows the consent URL to the person (starts a browser, or prints it).
	Open   func(authURL string) error
	Client *http.Client
	// Timeout bounds the wait for the browser (default 5 minutes).
	Timeout time.Duration
	// Endpoint overrides, for tests.
	AuthEndpoint  string
	TokenEndpoint string
}

// LoopbackFlow runs the browser flow and returns the stored-token blob.
func LoopbackFlow(ctx context.Context, o LoopbackOptions) (OAuthToken, error) {
	if o.ClientID == "" {
		return OAuthToken{}, fmt.Errorf("a client id is required (see docs/CONNECTORS.md)")
	}
	if o.Open == nil {
		return OAuthToken{}, fmt.Errorf("no way to show the consent page")
	}
	authEP, tokenEP := o.AuthEndpoint, o.TokenEndpoint
	if authEP == "" {
		authEP = AuthURL(o.Provider, o.Tenant)
	}
	if tokenEP == "" {
		tokenEP = TokenURL(o.Provider, o.Tenant)
	}
	if authEP == "" || tokenEP == "" {
		return OAuthToken{}, fmt.Errorf("%q has no browser flow", o.Provider)
	}
	// Loopback by IP, not "localhost": it cannot be redirected by a hosts
	// file, and it is the form both providers document.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return OAuthToken{}, err
	}
	defer ln.Close()
	redirect := fmt.Sprintf("http://%s/callback", ln.Addr().String())

	verifier, state := randString(48), randString(24)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id": {o.ClientID}, "response_type": {"code"}, "redirect_uri": {redirect},
		"scope": {strings.Join(o.Scopes, " ")}, "state": {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
	}
	if o.Provider == "google" {
		// offline + consent is what makes Google return a refresh token at all.
		q.Set("access_type", "offline")
		q.Set("prompt", "consent")
	}
	authURL := authEP + "?" + q.Encode()

	type answer struct{ code, err string }
	got := make(chan answer, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		v := r.URL.Query()
		a := answer{code: v.Get("code")}
		switch {
		case v.Get("state") != state:
			a = answer{err: "state mismatch (this response is not for this request)"}
		case v.Get("error") != "":
			a = answer{err: v.Get("error") + ": " + v.Get("error_description")}
		case a.code == "":
			a = answer{err: "no authorization code in the response"}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if a.err != "" {
			fmt.Fprintf(w, "<p>Grimoire could not connect: %s</p>", html.EscapeString(a.err))
		} else {
			fmt.Fprint(w, "<p>Connected. You can close this tab and return to the terminal.</p>")
		}
		select {
		case got <- a:
		default:
		}
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(ln)
	defer srv.Close()

	if err := o.Open(authURL); err != nil {
		return OAuthToken{}, err
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	var ans answer
	select {
	case ans = <-got:
	case <-time.After(timeout):
		return OAuthToken{}, fmt.Errorf("timed out waiting for the browser")
	case <-ctx.Done():
		return OAuthToken{}, ctx.Err()
	}
	if ans.err != "" {
		return OAuthToken{}, fmt.Errorf("%s", ans.err)
	}

	form := url.Values{"grant_type": {"authorization_code"}, "code": {ans.code}, "redirect_uri": {redirect},
		"client_id": {o.ClientID}, "code_verifier": {verifier}}
	if o.ClientSecret != "" {
		form.Set("client_secret", o.ClientSecret)
	}
	if o.Provider == "microsoft" {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	resp, err := postForm(ctx, o.Client, tokenEP, form)
	if err != nil {
		return OAuthToken{}, fmt.Errorf("exchanging the code: %w", err)
	}
	t := OAuthToken{Provider: o.Provider, AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken,
		ClientID: o.ClientID, ClientSecret: o.ClientSecret, Tenant: o.Tenant, Scopes: o.Scopes}
	if resp.ExpiresIn > 0 {
		t.Expiry = rfc3339(timeNow().Add(time.Duration(resp.ExpiresIn) * time.Second))
	}
	if t.RefreshToken == "" {
		return t, fmt.Errorf("the provider returned no refresh token, so the connection would expire in an hour. " +
			"Remove Grimoire from the account's connected apps and run connect again")
	}
	return t, nil
}

// DeviceOptions configures GitHub's device flow.
type DeviceOptions struct {
	ClientID string
	Scope    string
	Show     func(userCode, verifyURL string)
	Client   *http.Client
	// Endpoint overrides, for tests. Poll is the minimum wait between polls.
	CodeEndpoint  string
	TokenEndpoint string
	Poll          time.Duration
}

// DeviceFlow runs GitHub's device authorization flow and returns the token.
func DeviceFlow(ctx context.Context, o DeviceOptions) (string, error) {
	codeEP, tokenEP := o.CodeEndpoint, o.TokenEndpoint
	if codeEP == "" {
		codeEP = "https://github.com/login/device/code"
	}
	if tokenEP == "" {
		tokenEP = "https://github.com/login/oauth/access_token"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codeEP,
		strings.NewReader(url.Values{"client_id": {o.ClientID}, "scope": {o.Scope}}.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var dc struct {
		DeviceCode      string `json:"device_code"`
		UserCode        string `json:"user_code"`
		VerificationURI string `json:"verification_uri"`
		ExpiresIn       int    `json:"expires_in"`
		Interval        int    `json:"interval"`
		Error           string `json:"error"`
	}
	if err := getJSONAllow(ctx, o.Client, req, &dc); err != nil {
		return "", err
	}
	if dc.Error != "" || dc.DeviceCode == "" {
		return "", fmt.Errorf("github refused the device flow: %s (is the OAuth app's device flow enabled?)", dc.Error)
	}
	o.Show(dc.UserCode, dc.VerificationURI)
	wait := time.Duration(dc.Interval) * time.Second
	if o.Poll > 0 {
		wait = o.Poll
	}
	if wait <= 0 {
		wait = 5 * time.Second
	}
	deadline := timeNow().Add(time.Duration(dc.ExpiresIn) * time.Second)
	if dc.ExpiresIn == 0 {
		deadline = timeNow().Add(15 * time.Minute)
	}
	for timeNow().Before(deadline) {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return "", ctx.Err()
		}
		resp, err := postForm(ctx, o.Client, tokenEP, url.Values{"client_id": {o.ClientID},
			"device_code": {dc.DeviceCode}, "grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}})
		if err == nil {
			return resp.AccessToken, nil
		}
		if strings.HasPrefix(err.Error(), "authorization_pending") || strings.HasPrefix(err.Error(), "slow_down") {
			if strings.HasPrefix(err.Error(), "slow_down") {
				wait += 5 * time.Second
			}
			continue
		}
		return "", err
	}
	return "", fmt.Errorf("the device code expired before it was approved")
}

func randString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)[:n]
}

// NewID is a random connector id.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Provision creates (or replaces, by kind+secret+name) a connector with
// safe defaults: hourly sync, external trust, no agent actions beyond those
// the caller names.
func Provision(store *Store, kind, secret string, cfg Config, name string) (Connector, error) {
	src, err := Get(kind)
	if err != nil {
		return Connector{}, err
	}
	if err := Validate(kind, cfg); err != nil {
		return Connector{}, err
	}
	d := src.Describe()
	if name == "" {
		name = d.Name
	}
	existing, _ := store.List()
	for _, c := range existing {
		if c.Kind == kind && c.Name == name && c.Secret == secret {
			c.Config = cfg
			return c, store.Save(c)
		}
	}
	c := Connector{ID: NewID(), Kind: kind, Name: name, Config: cfg, Secret: secret,
		Prefix: d.DefaultPrefix, Interval: 3600, Enabled: true, LastOK: true, Created: rfc3339(timeNow())}
	return c, store.Save(c)
}
