package oauth

import (
	"net/url"
	"strings"
)

// RedirectAllowlist bounds which hosts Dynamic Client Registration may claim
// a redirect_uri on.
//
// This is a HOST allowlist, not a hardcoded exact-path allowlist, on
// purpose. The MCP authorization spec and both vendors' own docs were
// checked (see docs/web-connectors.md) and neither Anthropic nor OpenAI
// publishes a stable, versioned callback PATH for their web connector OAuth
// flow — community write-ups quote paths like
// "https://claude.ai/api/mcp/auth_callback", but nothing under
// support.claude.com, platform.claude.com or developers.openai.com commits
// to one, and a wrong guess here would either lock out a legitimate
// connector or (worse) be copied verbatim into a security control. Binding
// to the HOST instead is both safer and more correct: Dynamic Client
// Registration is exactly the mechanism that lets the client tell us its
// real redirect_uri at connect time, so the server's job is bounding WHO may
// register (a small set of trusted hosts) rather than guessing WHERE they
// redirect to today. /oauth/authorize then holds each client to the exact
// URI it registered — see server.go — so the host allowlist is the outer
// gate and exact match is the inner one.
type RedirectAllowlist struct {
	patterns []string
}

// DefaultRedirectPatterns are the hosts trusted out of the box: claude.ai
// and its subdomains, claude.com (Anthropic's newer domain, also seen in the
// custom-connector flow), chatgpt.com and openai.com subdomains, and
// localhost/127.0.0.1 on any port for the MCP Inspector and other local test
// clients. GRIMOIRE_OAUTH_ALLOWED_REDIRECTS replaces this list; it does not
// merge with it, so an operator who wants Inspector testing alongside a
// custom set must re-list "http://localhost" themselves.
func DefaultRedirectPatterns() []string {
	return []string{
		"https://claude.ai",
		"https://*.claude.ai",
		"https://claude.com",
		"https://*.claude.com",
		"https://chatgpt.com",
		"https://*.chatgpt.com",
		"https://*.openai.com",
		"http://localhost",
		"http://127.0.0.1",
	}
}

// NewRedirectAllowlist builds an allowlist from scheme://host patterns. A
// pattern's host may start with "*." to match any subdomain (but not the
// bare apex — list both explicitly, as DefaultRedirectPatterns does).
func NewRedirectAllowlist(patterns []string) *RedirectAllowlist {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return &RedirectAllowlist{patterns: out}
}

// Allowed reports whether rawURI may be registered as a redirect_uri.
//
// Independent of the pattern match, OAuth 2.1's communication-security
// requirement is enforced directly: a redirect URI must be either
// "localhost"/"127.0.0.1"/"::1" or use https. A pattern list that somehow
// contained a bare-http non-loopback entry could not override that.
func (a *RedirectAllowlist) Allowed(rawURI string) bool {
	if a == nil {
		return false
	}
	u, err := url.Parse(rawURI)
	if err != nil || u.Host == "" || u.Fragment != "" {
		return false
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return false
	}
	for _, p := range a.patterns {
		pu, err := url.Parse(p)
		if err != nil || pu.Scheme != u.Scheme {
			continue
		}
		ph := pu.Hostname()
		if strings.HasPrefix(ph, "*.") {
			suffix := ph[1:] // ".claude.ai"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
			continue
		}
		if ph == host {
			return true // port intentionally ignored: a pattern names a host, not an endpoint
		}
	}
	return false
}
