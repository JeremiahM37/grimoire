package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// OAuth credentials for connectors.
//
// `grimoire connect google|microsoft` stores ONE vault secret per provider
// holding a JSON token blob: access token, refresh token, expiry, and the
// client id (and secret, for Google) needed to refresh it. A connector names
// that secret like any other credential, and ResolveToken hands the connector
// code a fresh access token. The blob never leaves the server: not into a
// note, not through the API, not to an agent. Refresh is automatic and the
// refreshed blob is written back, so a connector keeps working past the
// access token's hour without anyone touching it.

// OAuthToken is the stored blob.
type OAuthToken struct {
	Provider     string   `json:"provider"`
	AccessToken  string   `json:"access_token"`
	RefreshToken string   `json:"refresh_token,omitempty"`
	Expiry       string   `json:"expiry,omitempty"` // RFC3339; empty = does not expire
	ClientID     string   `json:"client_id,omitempty"`
	ClientSecret string   `json:"client_secret,omitempty"`
	Tenant       string   `json:"tenant,omitempty"`
	Scopes       []string `json:"scopes,omitempty"`
}

// Encode renders the blob as the string the vault stores.
func (t OAuthToken) Encode() string {
	b, _ := json.Marshal(t)
	return string(b)
}

// ParseOAuth recognises a stored blob. A plain token (a PAT, a bot token, an
// app password) is not one, and passes through ResolveToken untouched, so
// every credential configured before this existed keeps working.
func ParseOAuth(raw string) (OAuthToken, bool) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "{") {
		return OAuthToken{}, false
	}
	var t OAuthToken
	if err := json.Unmarshal([]byte(raw), &t); err != nil || t.Provider == "" || t.AccessToken == "" {
		return OAuthToken{}, false
	}
	return t, true
}

// Expired reports whether the access token needs refreshing (with a minute of
// slack so it does not die mid-request).
func (t OAuthToken) Expired(now time.Time) bool {
	if t.Expiry == "" {
		return false
	}
	exp, err := time.Parse(time.RFC3339, t.Expiry)
	if err != nil {
		return true
	}
	return now.Add(time.Minute).After(exp)
}

// TokenURL is the provider's token endpoint.
func (t OAuthToken) TokenURL() string { return TokenURL(t.Provider, t.Tenant) }

// TokenURL returns a provider's token endpoint.
func TokenURL(provider, tenant string) string {
	switch provider {
	case "google":
		return "https://oauth2.googleapis.com/token"
	case "microsoft":
		if tenant == "" {
			tenant = "common"
		}
		return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/token"
	}
	return ""
}

// AuthURL returns a provider's authorization endpoint.
func AuthURL(provider, tenant string) string {
	switch provider {
	case "google":
		return "https://accounts.google.com/o/oauth2/v2/auth"
	case "microsoft":
		if tenant == "" {
			tenant = "common"
		}
		return "https://login.microsoftonline.com/" + url.PathEscape(tenant) + "/oauth2/v2.0/authorize"
	}
	return ""
}

// Refresh exchanges the refresh token for a new access token.
func Refresh(ctx context.Context, client *http.Client, t OAuthToken) (OAuthToken, error) {
	if t.RefreshToken == "" {
		return t, fmt.Errorf("the %s token has expired and has no refresh token; run `grimoire connect %s` again", t.Provider, t.Provider)
	}
	endpoint := t.TokenURL()
	if endpoint == "" {
		return t, fmt.Errorf("%s tokens cannot be refreshed", t.Provider)
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {t.RefreshToken}, "client_id": {t.ClientID}}
	if t.ClientSecret != "" {
		form.Set("client_secret", t.ClientSecret)
	}
	if t.Provider == "microsoft" && len(t.Scopes) > 0 {
		form.Set("scope", strings.Join(t.Scopes, " "))
	}
	resp, err := postForm(ctx, client, endpoint, form)
	if err != nil {
		return t, fmt.Errorf("refreshing the %s token: %w", t.Provider, err)
	}
	t.AccessToken = resp.AccessToken
	if resp.RefreshToken != "" {
		t.RefreshToken = resp.RefreshToken
	}
	t.Expiry = ""
	if resp.ExpiresIn > 0 {
		t.Expiry = rfc3339(timeNow().Add(time.Duration(resp.ExpiresIn) * time.Second))
	}
	return t, nil
}

// TokenResponse is the standard OAuth token reply.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (TokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out TokenResponse
	if err := getJSONAllow(ctx, client, req, &out); err != nil {
		return out, err
	}
	if out.Error != "" {
		return out, fmt.Errorf("%s: %s", out.Error, out.Description)
	}
	if out.AccessToken == "" {
		return out, fmt.Errorf("the token endpoint returned no access token")
	}
	return out, nil
}

// ExchangeForm is the exported form post used by the connect flow.
func ExchangeForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) (TokenResponse, error) {
	return postForm(ctx, client, endpoint, form)
}

// getJSONAllow decodes the body even on a 4xx: token endpoints report errors
// as JSON ({"error":"invalid_grant",...}) that is worth showing verbatim.
func getJSONAllow(ctx context.Context, c *http.Client, req *http.Request, out any) error {
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Host, err)
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(http_LimitReader(resp))
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: status %d, response was not JSON", req.URL.Host, resp.StatusCode)
	}
	return nil
}

// SecretWriter is implemented by a secret store that can persist a refreshed token.
type SecretWriter interface {
	Put(name, value string, meta map[string]any) error
}

var refreshMu sync.Mutex

// ResolveToken returns the credential a connector should present. For a plain
// secret that is the value itself; for an OAuth blob it is a fresh access
// token, refreshed (and written back) if it was about to expire.
func ResolveToken(ctx context.Context, s Secrets, name string, client *http.Client) (string, error) {
	raw, err := s.Get(name)
	if err != nil {
		return "", err
	}
	t, ok := ParseOAuth(raw)
	if !ok {
		return raw, nil
	}
	if !t.Expired(timeNow()) {
		return t.AccessToken, nil
	}
	// Serialised: two syncs noticing the same expiry would both refresh, and
	// providers that rotate refresh tokens invalidate the loser's.
	refreshMu.Lock()
	defer refreshMu.Unlock()
	if raw2, err := s.Get(name); err == nil {
		if t2, ok := ParseOAuth(raw2); ok && !t2.Expired(timeNow()) {
			return t2.AccessToken, nil
		}
	}
	nt, err := Refresh(ctx, client, t)
	if err != nil {
		return "", err
	}
	if w, ok := s.(SecretWriter); ok {
		if err := w.Put(name, nt.Encode(), map[string]any{"kind": "oauth", "provider": nt.Provider}); err != nil {
			return "", fmt.Errorf("saving the refreshed token: %w", err)
		}
	}
	return nt.AccessToken, nil
}

// ---- scopes

// ProviderOf names the OAuth provider a connector kind authenticates with.
func ProviderOf(kind string) string {
	switch kind {
	case "gmail", "gdrive", "gcal":
		return "google"
	case "outlook", "onedrive":
		return "microsoft"
	case "slack", "github":
		return kind
	}
	return ""
}

// readScopes are the read-only scopes each kind needs. These are what
// `grimoire connect` requests by default; write scopes are added only for an
// action the operator names with --allow.
var readScopes = map[string][]string{
	"gmail":    {"https://www.googleapis.com/auth/gmail.readonly"},
	"gdrive":   {"https://www.googleapis.com/auth/drive.readonly"},
	"gcal":     {"https://www.googleapis.com/auth/calendar.readonly"},
	"outlook":  {"Mail.Read"},
	"onedrive": {"Files.Read"},
}

// ScopesFor returns the scopes to request for the given services (connector
// kinds) and allowed actions ("gmail.create_draft"). Unknown names are an
// error rather than ignored: a typo silently dropping a scope would fail much
// later, as a 403 on the first action.
func ScopesFor(provider string, services, allow []string) ([]string, error) {
	set := map[string]bool{}
	for _, svc := range services {
		if ProviderOf(svc) != provider {
			return nil, fmt.Errorf("%q is not a %s service", svc, provider)
		}
		for _, sc := range readScopes[svc] {
			set[sc] = true
		}
	}
	for _, a := range allow {
		kind, name, ok := strings.Cut(a, ".")
		if !ok {
			return nil, fmt.Errorf("--allow takes kind.action, like gmail.create_draft (got %q)", a)
		}
		if ProviderOf(kind) != provider {
			return nil, fmt.Errorf("%q is not a %s action", a, provider)
		}
		src, err := Get(kind)
		if err != nil {
			return nil, err
		}
		actor, isActor := src.(Actor)
		if !isActor {
			return nil, fmt.Errorf("%s has no actions", kind)
		}
		found := false
		for _, spec := range actor.Actions() {
			if spec.Name == name {
				found = true
				for _, sc := range spec.Scopes {
					set[sc] = true
				}
				// An action implies reading its own service.
				for _, sc := range readScopes[kind] {
					set[sc] = true
				}
			}
		}
		if !found {
			return nil, fmt.Errorf("%s has no action %q", kind, name)
		}
	}
	if provider == "microsoft" {
		set["offline_access"], set["User.Read"] = true, true
	}
	out := make([]string, 0, len(set))
	for sc := range set {
		out = append(out, sc)
	}
	sort.Strings(out)
	return out, nil
}
