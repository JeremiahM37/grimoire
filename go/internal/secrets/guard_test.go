package secrets

import (
	"net/url"
	"testing"
)

// scopeAllows is the check that decides whether a credential is attached to a
// caller-supplied URL, so it gets a table of its own rather than being covered
// only through the broker. Each row is a URL shape that a prefix comparison
// would have gotten wrong.

func TestScopeAllows(t *testing.T) {
	cases := []struct {
		name   string
		scope  string
		target string
		allow  bool
	}{
		{"empty scope is unrestricted", "", "https://anything.example/x", true},
		{"exact match", "https://api.example.com", "https://api.example.com", true},
		{"path below scope", "https://api.example.com", "https://api.example.com/v1/x", true},
		{"query string is not part of the path",
			"https://api.example.com/v1", "https://api.example.com/v1?q=1", true},
		{"trailing slash in scope", "https://api.example.com/v1/", "https://api.example.com/v1/x", true},
		{"default port on target", "https://api.example.com:443/v1", "https://api.example.com/v1/x", true},
		{"default port on scope", "https://api.example.com/v1", "https://api.example.com:443/v1/x", true},
		{"host case is insensitive", "https://API.example.com", "https://api.example.com/x", true},

		// The bypasses. Every one of these satisfies strings.HasPrefix.
		{"host suffix", "https://api.example.com", "https://api.example.com.evil.test/x", false},
		{"host suffix with dash", "https://api.example.com", "https://api.example.com-evil.test/x", false},
		{"scope appears in the path", "https://api.example.com",
			"https://evil.test/https://api.example.com/x", false},
		{"path segment extended", "https://api.example.com/v1", "https://api.example.com/v10/x", false},
		{"userinfo host confusion", "https://api.example.com",
			"https://api.example.com@evil.test/x", false},

		// Origin components each have to match.
		{"scheme downgrade", "https://api.example.com", "http://api.example.com/x", false},
		{"different port", "https://api.example.com", "https://api.example.com:8443/x", false},
		{"different host", "https://api.example.com", "https://other.example.com/x", false},
		{"sibling path", "https://api.example.com/v1", "https://api.example.com/v2", false},
		{"parent path", "https://api.example.com/v1/sub", "https://api.example.com/v1", false},
	}
	for _, c := range cases {
		err := scopeAllows(c.scope, c.target)
		if c.allow && err != nil {
			t.Errorf("%s: scope %q should allow %q, got %v", c.name, c.scope, c.target, err)
		}
		if !c.allow && err == nil {
			t.Errorf("%s: scope %q must NOT allow %q", c.name, c.scope, c.target)
		}
	}
}

func TestPathWithinComparesWholeSegments(t *testing.T) {
	cases := []struct {
		scope, target string
		want          bool
	}{
		{"", "/anything", true},
		{"/", "/anything", true},
		{"/v1", "/v1", true},
		{"/v1", "/v1/x", true},
		{"/v1/", "/v1/x", true},
		{"/v1", "/v10", false},
		{"/v1", "/v1x", false},
		{"/v1", "/v", false},
		{"/v1", "/", false},
	}
	for _, c := range cases {
		if got := pathWithin(c.scope, c.target); got != c.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", c.scope, c.target, got, c.want)
		}
	}
}

func TestAllowedScheme(t *testing.T) {
	for _, ok := range []string{"http://x/y", "https://x/y", "HTTPS://x/y"} {
		if err := allowedSchemeString(t, ok); err != nil {
			t.Errorf("%s should be brokerable: %v", ok, err)
		}
	}
	for _, bad := range []string{"file:///etc/passwd", "gopher://x/", "ftp://x/", "data:text/plain,x"} {
		if err := allowedSchemeString(t, bad); err == nil {
			t.Errorf("%s must not be brokerable", bad)
		}
	}
}

// allowedSchemeString parses then checks, so the table above can be written as
// URLs rather than as pre-parsed values.
func allowedSchemeString(t *testing.T, raw string) error {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return allowedScheme(u)
}
