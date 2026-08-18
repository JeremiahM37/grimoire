package secrets

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// Outbound guards for the broker.
//
// The broker is the one place in grimoire that attaches a credential to a
// request aimed at a caller-supplied URL, which makes it the one place where
// getting URL comparison subtly wrong hands the credential to somebody else.
// Two independent controls apply, because either alone has a known gap:
//
//   - scopeAllows compares ORIGINS, not string prefixes;
//   - the dial guard checks the IP actually being connected to, after DNS and
//     on every redirect hop, so a name that resolves inward cannot be reached.

// ErrOutOfScope is returned when a target falls outside a grant's scope.
var ErrOutOfScope = errors.New("url outside grant scope")

// scopeAllows reports whether target is inside scope.
//
// The rule is origin-exact plus path-prefix on whole segments. A raw
// strings.HasPrefix — which is what this replaced — treats the scope as an
// opaque string, so a grant for "https://api.github.com" also matched
// "https://api.github.com.evil.example/steal": same prefix, entirely different
// host. It equally matched "https://evil.test/https://api.github.com/...",
// where the scope appears in the PATH. Both send the credential to the
// attacker. Comparing parsed origins is the only version of this check that
// means what it says.
func scopeAllows(scope, target string) error {
	if strings.TrimSpace(scope) == "" {
		return nil // an unscoped grant is deliberately unrestricted
	}
	s, err := url.Parse(scope)
	if err != nil {
		return fmt.Errorf("%w: unparseable scope", ErrOutOfScope)
	}
	t, err := url.Parse(target)
	if err != nil {
		return fmt.Errorf("%w: unparseable target", ErrOutOfScope)
	}
	if !strings.EqualFold(s.Scheme, t.Scheme) {
		return fmt.Errorf("%w %q: scheme differs", ErrOutOfScope, scope)
	}
	if !strings.EqualFold(s.Hostname(), t.Hostname()) {
		return fmt.Errorf("%w %q: host differs", ErrOutOfScope, scope)
	}
	if defaultedPort(s) != defaultedPort(t) {
		return fmt.Errorf("%w %q: port differs", ErrOutOfScope, scope)
	}
	if !pathWithin(s.Path, t.Path) {
		return fmt.Errorf("%w %q: path outside prefix", ErrOutOfScope, scope)
	}
	return nil
}

// defaultedPort normalizes the implicit port so that "https://h" and
// "https://h:443" are recognized as the same origin.
func defaultedPort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return "443"
	case "http":
		return "80"
	}
	return ""
}

// pathWithin reports whether target's path is the scope path or below it,
// comparing whole segments so that a scope of "/v1" does not authorize "/v10".
func pathWithin(scopePath, targetPath string) bool {
	scopePath = strings.TrimSuffix(scopePath, "/")
	if scopePath == "" {
		return true
	}
	if targetPath == scopePath {
		return true
	}
	return strings.HasPrefix(targetPath, scopePath+"/")
}

// allowedScheme restricts the broker to the two schemes it can meaningfully
// speak. SECURITY.md states this; enforcing it explicitly means it does not
// depend on which transports happen to be registered.
func allowedScheme(u *url.URL) error {
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return nil
	}
	return fmt.Errorf("scheme %q is not brokerable", u.Scheme)
}

// blockedIP reports why an address must not be reached, or nil if it may be.
//
// allowPrivate is the self-hoster's escape hatch (GRIMOIRE_BROKER_ALLOW_PRIVATE):
// brokering to a LAN service is a legitimate thing to want. Cloud-metadata and
// link-local are refused regardless, because there is no legitimate reason to
// broker a credential at 169.254.169.254 and every reason an attacker would.
func blockedIP(ip net.IP, allowPrivate bool) error {
	if ip == nil {
		return errors.New("unresolvable address")
	}
	switch {
	case ip.IsUnspecified():
		return fmt.Errorf("refusing unspecified address %s", ip)
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		// 169.254.0.0/16 and fe80::/10 — includes cloud metadata.
		return fmt.Errorf("refusing link-local address %s", ip)
	case ip.IsMulticast():
		return fmt.Errorf("refusing multicast address %s", ip)
	case ip.IsInterfaceLocalMulticast():
		return fmt.Errorf("refusing interface-local address %s", ip)
	}
	for _, n := range alwaysBlocked {
		if n.Contains(ip) {
			return fmt.Errorf("refusing reserved address %s", ip)
		}
	}
	if allowPrivate {
		return nil
	}
	if ip.IsLoopback() || ip.IsPrivate() {
		return fmt.Errorf("refusing internal address %s "+
			"(set GRIMOIRE_BROKER_ALLOW_PRIVATE=1 to broker to your own network)", ip)
	}
	for _, n := range privateExtra {
		if n.Contains(ip) {
			return fmt.Errorf("refusing internal address %s "+
				"(set GRIMOIRE_BROKER_ALLOW_PRIVATE=1 to broker to your own network)", ip)
		}
	}
	return nil
}

func mustCIDRs(specs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(specs))
	for _, s := range specs {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic("bad CIDR in guard table: " + s)
		}
		out = append(out, n)
	}
	return out
}

// Ranges that are never a legitimate broker target, even for a self-hoster:
// IETF protocol assignments, documentation and benchmarking space, and the
// IPv4-mapped forms that would otherwise slip past an IPv4 check.
var alwaysBlocked = mustCIDRs(
	"192.0.0.0/24",
	"192.0.2.0/24",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"::/128",
	"64:ff9b::/96",
)

// Ranges treated as internal but reachable with the opt-in: carrier-grade NAT
// and unique-local IPv6, which net.IP.IsPrivate does not cover.
var privateExtra = mustCIDRs("100.64.0.0/10", "fc00::/7")

// guardedTransport builds the broker's HTTP transport with an address check
// that runs at CONNECT time.
//
// Checking here rather than by resolving the hostname up front is what closes
// DNS rebinding: the address handed to Control is the one the socket is about
// to use, so a name that answered publicly on the first lookup and privately
// on the second is still refused. It also covers every redirect hop for free,
// since each hop dials again.
func guardedTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		Control: func(network, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			return blockedIP(net.ParseIP(host), allowPrivate)
		},
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}

// allowPrivateFromEnv reads the documented opt-in.
func allowPrivateFromEnv() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("GRIMOIRE_BROKER_ALLOW_PRIVATE"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
