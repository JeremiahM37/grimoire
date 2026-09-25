package oauth

import "testing"

func TestRedirectAllowlist(t *testing.T) {
	a := NewRedirectAllowlist(DefaultRedirectPatterns())

	allowed := []string{
		"https://claude.ai/api/mcp/auth_callback",
		"https://some-org.claude.ai/callback",
		"https://claude.com/oauth/callback",
		"https://chatgpt.com/connector_platform_oauth_redirect",
		"https://some.subdomain.openai.com/callback",
		"http://localhost:6274/callback",
		"http://127.0.0.1:33418/callback",
	}
	for _, uri := range allowed {
		if !a.Allowed(uri) {
			t.Errorf("expected %q to be allowed", uri)
		}
	}

	denied := []string{
		"https://evil.example.com/callback",
		// bare-http, non-loopback: OAuth 2.1 requires https or localhost.
		"http://claude.ai/callback",
		// a suffix match that is not actually a subdomain.
		"https://notclaude.ai/callback",
		"https://evilclaude.ai/callback",
		// no host at all.
		"not-a-url",
		// a fragment, which RFC 6749/8252 redirect URIs must not carry.
		"https://claude.ai/callback#frag",
	}
	for _, uri := range denied {
		if a.Allowed(uri) {
			t.Errorf("expected %q to be denied", uri)
		}
	}
}

func TestRedirectAllowlistCustomReplacesDefault(t *testing.T) {
	a := NewRedirectAllowlist([]string{"https://my-agent.example.com"})
	if a.Allowed("https://claude.ai/callback") {
		t.Fatal("a custom allowlist must not silently keep the defaults")
	}
	if !a.Allowed("https://my-agent.example.com/cb") {
		t.Fatal("the custom entry itself should be allowed")
	}
}
