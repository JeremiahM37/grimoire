// Package identity answers "who is asking", for callers that are not on this
// machine.
//
// Grimoire already had two ideas of a caller and they did not meet. A
// Principal is authenticated and decides what may be read. An agent name is a
// header the caller sets about itself, used for attribution — the ledger, the
// read-audit trail, the authority lattice that decides whether a human's
// correction outranks an agent's rewrite. On loopback that gap is harmless
// because the only caller is the operator. It stops being harmless the moment
// agents run on other devices, because every one of those mechanisms is keyed
// on who said something, and nothing was checking.
//
// An overlay network already knows the answer. A packet arriving on a
// WireGuard or ZeroTier interface has been authenticated by that network's own
// cryptography before Grimoire ever sees it, and the local daemon can name the
// peer. This package asks it, so attribution stops being a claim.
//
// Everything here is optional and off by default. With no backend configured
// the resolver identifies nobody, callers fall back to the self-asserted name
// exactly as before, and a deployment that never heard of any of this is
// unaffected. That is deliberate: an identity system that must be configured
// before the software works is a barrier, and most people run this on one box.
package identity

import (
	"net/http"
	"net/netip"
	"sort"
	"strings"
)

// Identity is who a request is from, as far as something other than the
// request itself could establish.
type Identity struct {
	// Subject is a stable, backend-qualified id — "tailscale:jam@github",
	// "zerotier:8bd5124fd6". Qualified because two backends can both produce
	// "laptop" and they are not the same principal.
	Subject string `json:"subject"`
	// Device names the machine. Empty when the backend cannot tell.
	Device string `json:"device,omitempty"`
	// User names the human the device is registered to, where the backend
	// tracks one.
	User string `json:"user,omitempty"`
	// Backend is which mechanism answered.
	Backend string `json:"backend"`
	// Verified is true only when the caller was authenticated by something
	// other than its own assertion. A backend that reads a header sets this
	// false unless it also established that the header came from somewhere
	// entitled to send it.
	Verified bool `json:"verified"`
}

// Backend resolves one kind of caller.
//
// peer is the address the TCP stack accepted the connection from, never a
// forwarded header. Every backend here is ultimately address-based, so a
// backend handed a caller-supplied address would authenticate whatever the
// caller typed.
type Backend interface {
	// Name is the identifier used in configuration and reported in results.
	Name() string
	// Identify returns the caller's identity, or ok=false to pass.
	Identify(peer netip.Addr, r *http.Request) (Identity, bool)
}

// Resolver asks each configured backend in turn.
type Resolver struct {
	backends []Backend
}

// New returns a resolver over backends, in the order given. A resolver with no
// backends identifies nobody, which is the default deployment.
func New(backends ...Backend) *Resolver {
	out := make([]Backend, 0, len(backends))
	for _, b := range backends {
		if b != nil {
			out = append(out, b)
		}
	}
	return &Resolver{backends: out}
}

// Enabled reports whether any backend is configured.
func (r *Resolver) Enabled() bool { return r != nil && len(r.backends) > 0 }

// Names lists the configured backends, for the console and for /whoami. An
// operator who cannot see which mechanisms are live cannot tell a working
// configuration from a silently inert one.
func (r *Resolver) Names() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.backends))
	for _, b := range r.backends {
		out = append(out, b.Name())
	}
	return out
}

// Identify returns the first identity a backend can establish.
//
// First match wins rather than best match: the order is the operator's
// configuration, and a resolver that silently preferred a different backend
// than the one listed first would be deciding policy it was not given.
func (r *Resolver) Identify(req *http.Request) (Identity, bool) {
	if r == nil || req == nil || len(r.backends) == 0 {
		return Identity{}, false
	}
	peer, ok := PeerAddr(req)
	if !ok {
		return Identity{}, false
	}
	for _, b := range r.backends {
		if id, ok := b.Identify(peer, req); ok && id.Subject != "" {
			id.Backend = b.Name()
			return id, true
		}
	}
	return Identity{}, false
}

// PeerAddr is the address the connection was actually accepted from.
//
// Deliberately NOT clientAddr: that one honours X-Forwarded-For when a proxy
// is trusted, which is right for rate limiting and catastrophic here. Every
// backend in this package decides identity from the address, so reading a
// caller-supplied one would let anybody claim to be any node on the overlay by
// setting a header. The peer address is the one the TCP handshake proved.
func PeerAddr(r *http.Request) (netip.Addr, bool) {
	if r == nil {
		return netip.Addr{}, false
	}
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap().WithZone(""), true
	}
	// A listener on a unix socket, or a synthesised request in a test, has an
	// address with no port.
	if a, err := netip.ParseAddr(strings.Trim(r.RemoteAddr, "[]")); err == nil {
		return a.Unmap().WithZone(""), true
	}
	return netip.Addr{}, false
}

// InAny reports whether addr falls in any prefix.
//
// Used as a guard in front of every address-based backend. The overlay
// networks assign from their own ranges, so an address outside them did not
// arrive over the overlay however convincing the lookup that follows might be.
func InAny(addr netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.IsValid() && p.Contains(addr) {
			return true
		}
	}
	return false
}

// ParsePrefixes reads a comma-separated CIDR list, skipping anything
// unparseable rather than failing the whole configuration — one typo in a list
// of five should not silently disable identity for the other four. Bare
// addresses are accepted and read as single hosts, because writing
// "127.0.0.1" and meaning it is the common case.
func ParsePrefixes(s string) []netip.Prefix {
	var out []netip.Prefix
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if p, err := netip.ParsePrefix(part); err == nil {
			out = append(out, p)
			continue
		}
		if a, err := netip.ParseAddr(part); err == nil {
			out = append(out, netip.PrefixFrom(a.Unmap(), a.BitLen()))
		}
	}
	return out
}

// SortedNames returns names sorted, for stable output in tests and the UI.
func SortedNames(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// constErr is an error that is a constant, so package-level sentinels need no
// initialisation.
type constErr string

func (e constErr) Error() string { return string(e) }
