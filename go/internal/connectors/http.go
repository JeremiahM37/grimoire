package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// The shared HTTP plumbing every source uses.
//
// Two things are worth doing once rather than five times: reporting a failed
// call in a way an operator can act on, and never letting a slow or enormous
// response hold the sync open. A connector that fails with "unexpected status
// 401" and nothing else is a connector nobody can fix without reading its
// source.

// maxBody bounds one response. Sources are paged, so a body larger than this
// is a sign something is wrong rather than a document worth having.
const maxBody = 32 << 20

// getJSON performs a request and decodes JSON into out.
func getJSON(ctx context.Context, c *http.Client, req *http.Request, out any) error {
	if c == nil {
		c = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := c.Do(req.WithContext(ctx))
	if err != nil {
		return fmt.Errorf("%s %s: %w", req.Method, req.URL.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return statusError(req, resp.StatusCode, body)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%s: response was not JSON (%d bytes): %w",
			req.URL.Host, len(body), err)
	}
	return nil
}

// statusError turns an HTTP failure into something an operator can act on:
// what was called, what came back, and what that usually means.
func statusError(req *http.Request, code int, body []byte) error {
	hint := ""
	switch code {
	case http.StatusUnauthorized:
		hint = " — the credential was rejected; check the secret this connector uses"
	case http.StatusForbidden:
		hint = " — authenticated, but not allowed; check the token's scopes and the account's access to this space or project"
	case http.StatusNotFound:
		hint = " — check the site URL and the space/project/channel identifier"
	case http.StatusTooManyRequests:
		hint = " — rate limited; the next scheduled sync will resume from the cursor"
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	return fmt.Errorf("%s %s: %d%s: %s", req.Method, req.URL.Path, code, hint, snippet)
}

// jsonRequest builds a GET with the usual headers.
func jsonRequest(rawURL string, query url.Values, headers map[string]string) (*http.Request, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grimoire-connector")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

// baseURL normalizes a configured site URL: scheme required, no trailing slash.
func baseURL(raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return "", missing("site URL")
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	if _, err := url.Parse(raw); err != nil {
		return "", fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return raw, nil
}
