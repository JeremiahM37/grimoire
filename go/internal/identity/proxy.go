package identity

import (
	"net/http"
	"net/netip"
	"strings"
)

// Proxy identifies a caller from a header set by an authenticating reverse
// proxy — Authelia, oauth2-proxy, Cloudflare Access, an nginx auth_request.
//
// This is the deployment most self-hosters already have, so refusing to
// support it would push them back onto a shared token. But it is also the
// backend that is trivially forgeable if it is done wrong, and doing it wrong
// is the norm: a server that simply reads Remote-User lets ANY caller become
// any user by setting a header, which is worse than having no identity at all
// because it looks like security.
//
// So the header is honoured only when the connection came from an address the
// operator listed as a proxy, and that address is the peer the TCP stack
// accepted — never a forwarded one. With no trusted addresses configured this
// backend identifies nobody, which is the safe way to be misconfigured.
type Proxy struct {
	// From is the set of addresses entitled to assert identity. Required:
	// empty means the backend is inert.
	From []netip.Prefix
	// Header carries the user. Defaults to Remote-User, which is what
	// Authelia and nginx auth_request use.
	Header string
	// DeviceHeader optionally carries the calling machine or agent.
	DeviceHeader string
}

// DefaultProxyHeader is the header an authenticating proxy conventionally sets.
const DefaultProxyHeader = "Remote-User"

func (p *Proxy) Name() string { return "proxy" }

func (p *Proxy) Identify(peer netip.Addr, r *http.Request) (Identity, bool) {
	if r == nil || len(p.From) == 0 {
		return Identity{}, false
	}
	if !InAny(peer, p.From) {
		return Identity{}, false
	}
	h := p.Header
	if h == "" {
		h = DefaultProxyHeader
	}
	user := strings.TrimSpace(r.Header.Get(h))
	if user == "" {
		return Identity{}, false
	}
	device := ""
	if p.DeviceHeader != "" {
		device = strings.TrimSpace(r.Header.Get(p.DeviceHeader))
	}
	return Identity{
		Subject: "proxy:" + user,
		User:    user,
		Device:  device,
		Backend: "proxy",
		// Verified because the proxy authenticated the user and this
		// connection came from the proxy. The trust is in the operator's
		// From list; that is what makes the header mean anything.
		Verified: true,
	}, true
}
