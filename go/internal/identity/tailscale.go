package identity

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// Tailscale identifies callers by asking the local tailscaled who owns the
// peer address.
//
// This is the strongest answer available without running a certificate
// authority, and it costs the operator nothing: a tailnet address is only
// reachable through the WireGuard tunnel, so an address in the tailnet range
// arriving on this machine was authenticated by Tailscale's own key exchange
// before Grimoire saw the connection. Asking the daemon turns that into a
// name.
//
// Deliberately spoken over the LocalAPI rather than by embedding tsnet. tsnet
// would pull a large part of Tailscale into a binary whose whole claim is that
// it is one file with no runtime dependencies, and it would make Grimoire a
// tailnet node — a much bigger thing to be than a program that asks a question
// of a daemon the operator already runs.
type Tailscale struct {
	// Endpoint is where tailscaled's LocalAPI listens. A "unix:///path" URL
	// (the default) or an http:// base, which is what a containerised
	// tailscaled and the tests both need.
	Endpoint string
	// Ranges bounds which peers are even asked about. Defaults to Tailscale's
	// assigned ranges. A peer outside them did not arrive over the tunnel, so
	// no answer about it should be believed.
	Ranges []netip.Prefix
	// TTL is how long an answer is reused. Node identity changes rarely and a
	// lookup per request would put a socket round-trip in front of every read.
	TTL time.Duration

	HTTP *http.Client

	once  sync.Once
	mu    sync.Mutex
	cache map[netip.Addr]cached
}

type cached struct {
	id Identity
	ok bool
	at time.Time
}

// TailscaleRanges are the address ranges Tailscale assigns: 100.64.0.0/10 for
// IPv4 (CGNAT space, which Tailscale claims by convention) and fd7a:115c:a1e0::/48
// for IPv6.
var TailscaleRanges = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("fd7a:115c:a1e0::/48"),
}

// DefaultTailscaleSocket is where tailscaled listens on Linux.
const DefaultTailscaleSocket = "unix:///var/run/tailscale/tailscaled.sock"

func (t *Tailscale) Name() string { return "tailscale" }

func (t *Tailscale) init() {
	t.once.Do(func() {
		if t.Endpoint == "" {
			t.Endpoint = DefaultTailscaleSocket
		}
		if len(t.Ranges) == 0 {
			t.Ranges = TailscaleRanges
		}
		if t.TTL == 0 {
			t.TTL = 5 * time.Minute
		}
		t.cache = map[netip.Addr]cached{}
		if t.HTTP == nil {
			t.HTTP = &http.Client{Timeout: 3 * time.Second}
		}
		if path, isSock := strings.CutPrefix(t.Endpoint, "unix://"); isSock {
			// The LocalAPI over a unix socket still speaks HTTP, so only the
			// dial changes. The host in the URL is ignored by the dialer but
			// tailscaled checks it, hence the fixed name below.
			d := &net.Dialer{}
			t.HTTP = &http.Client{
				Timeout: 3 * time.Second,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
						return d.DialContext(ctx, "unix", path)
					},
				},
			}
		}
	})
}

// baseURL is the http base to send LocalAPI requests to.
func (t *Tailscale) baseURL() string {
	if strings.HasPrefix(t.Endpoint, "unix://") {
		// Any host works over the socket dialer; tailscaled requires this one.
		return "http://local-tailscaled.sock"
	}
	return strings.TrimRight(t.Endpoint, "/")
}

// whoisReply is the part of tailscaled's answer this needs.
type whoisReply struct {
	Node struct {
		Name     string `json:"Name"`
		Hostinfo struct {
			Hostname string `json:"Hostname"`
			OS       string `json:"OS"`
		} `json:"Hostinfo"`
	} `json:"Node"`
	UserProfile struct {
		LoginName   string `json:"LoginName"`
		DisplayName string `json:"DisplayName"`
	} `json:"UserProfile"`
}

func (t *Tailscale) Identify(peer netip.Addr, r *http.Request) (Identity, bool) {
	t.init()
	if !InAny(peer, t.Ranges) {
		return Identity{}, false
	}
	t.mu.Lock()
	if c, ok := t.cache[peer]; ok && time.Since(c.at) < t.TTL {
		t.mu.Unlock()
		return c.id, c.ok
	}
	t.mu.Unlock()

	id, ok := t.lookup(peer, r)
	t.mu.Lock()
	t.cache[peer] = cached{id: id, ok: ok, at: time.Now()}
	t.mu.Unlock()
	return id, ok
}

func (t *Tailscale) lookup(peer netip.Addr, r *http.Request) (Identity, bool) {
	ctx := context.Background()
	if r != nil && r.Context() != nil {
		ctx = r.Context()
	}
	// The port is required by the API but not used to identify the peer, so a
	// placeholder is honest here: what is being asked is "who owns this
	// address on the tailnet".
	url := fmt.Sprintf("%s/localapi/v0/whois?addr=%s", t.baseURL(),
		netip.AddrPortFrom(peer, 1).String())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Identity{}, false
	}
	// tailscaled rejects LocalAPI requests without this; it is a CSRF guard,
	// not authentication.
	req.Header.Set("Sec-Tailscale", "localapi")
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return Identity{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Identity{}, false
	}
	var out whoisReply
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Identity{}, false
	}

	device := strings.TrimSuffix(out.Node.Hostinfo.Hostname, ".")
	if device == "" {
		// Node.Name is the MagicDNS name, "laptop.tailnet.ts.net." — the first
		// label is the machine.
		name := strings.TrimSuffix(out.Node.Name, ".")
		device, _, _ = strings.Cut(name, ".")
	}
	user := out.UserProfile.LoginName
	if user == "" {
		user = out.UserProfile.DisplayName
	}
	// A node with neither a user nor a name is a reply this cannot make an
	// identity out of; saying so beats inventing "tailscale:".
	if user == "" && device == "" {
		return Identity{}, false
	}
	subject := user
	if subject == "" {
		subject = device
	}
	return Identity{
		Subject:  "tailscale:" + subject,
		Device:   device,
		User:     user,
		Backend:  "tailscale",
		Verified: true,
	}, true
}
