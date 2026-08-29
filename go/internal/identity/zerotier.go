package identity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// ZeroTier identifies callers by their managed address on a ZeroTier network.
//
// ZeroTier has no equivalent of `tailscale whois`: the local service knows its
// peers by node id and physical path, not by the address the network assigned
// them. The identity therefore comes from the network's member list — from
// ZeroTier Central, or from a self-hosted controller — which maps each
// member's node id and name to the addresses it was assigned.
//
// That makes this weaker than the Tailscale backend in one specific way worth
// naming rather than papering over: it is a mapping fetched on a schedule, not
// an answer about this connection. A member whose assignment changed inside
// the refresh window is identified as whoever held the address before. The
// window is short and assignments are stable, but the guarantee is "this
// address belongs to that member" rather than "this connection came from that
// member", and the Ranges check below is what keeps the first statement worth
// anything: an address outside the network's own assignment pool did not
// arrive over ZeroTier, whatever the map says.
type ZeroTier struct {
	// API is the controller base. ZeroTier Central by default; a self-hosted
	// controller is "http://localhost:9993".
	API string
	// Network is the 16-character network id.
	Network string
	// Token authenticates to the controller. Central wants an API token, a
	// self-hosted controller the contents of authtoken.secret; both headers
	// are sent because a controller ignores the one it does not use.
	Token string
	// Ranges bounds which peers are looked up at all. Set from the network's
	// assigned pool; empty means "any address the member list mentions", which
	// is weaker and is why configuring it is documented.
	Ranges []netip.Prefix
	// TTL is how long the member map is reused before refetching.
	TTL time.Duration

	HTTP *http.Client

	once  sync.Once
	mu    sync.Mutex
	byIP  map[netip.Addr]Identity
	at    time.Time
	fetch func(ctx context.Context) (map[netip.Addr]Identity, error)
}

// DefaultZeroTierAPI is ZeroTier Central.
const DefaultZeroTierAPI = "https://api.zerotier.com/api/v1"

func (z *ZeroTier) Name() string { return "zerotier" }

func (z *ZeroTier) init() {
	z.once.Do(func() {
		if z.API == "" {
			z.API = DefaultZeroTierAPI
		}
		if z.TTL == 0 {
			z.TTL = 2 * time.Minute
		}
		if z.HTTP == nil {
			z.HTTP = &http.Client{Timeout: 5 * time.Second}
		}
		if z.fetch == nil {
			z.fetch = z.fetchMembers
		}
	})
}

func (z *ZeroTier) Identify(peer netip.Addr, r *http.Request) (Identity, bool) {
	z.init()
	if z.Network == "" {
		return Identity{}, false
	}
	if len(z.Ranges) > 0 && !InAny(peer, z.Ranges) {
		return Identity{}, false
	}
	ctx := context.Background()
	if r != nil && r.Context() != nil {
		ctx = r.Context()
	}
	m := z.members(ctx)
	id, ok := m[peer]
	return id, ok
}

// members returns the address map, refreshing it when stale.
//
// A failed refresh keeps serving the previous map rather than dropping every
// caller's identity. A controller that is briefly unreachable should degrade
// to slightly stale attribution, not to a burst of unattributed writes.
func (z *ZeroTier) members(ctx context.Context) map[netip.Addr]Identity {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.byIP != nil && time.Since(z.at) < z.TTL {
		return z.byIP
	}
	m, err := z.fetch(ctx)
	if err != nil {
		if z.byIP != nil {
			// Back off a little so a hard-down controller is not refetched on
			// every request, but keep answering from what is known.
			z.at = time.Now().Add(-z.TTL).Add(15 * time.Second)
		}
		return z.byIP
	}
	z.byIP, z.at = m, time.Now()
	return z.byIP
}

// ztMember is the shape both Central and a self-hosted controller return, to
// the extent this needs it.
type ztMember struct {
	NodeID      string `json:"nodeId"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Config      struct {
		IPAssignments []string `json:"ipAssignments"`
		Authorized    bool     `json:"authorized"`
	} `json:"config"`
	// A self-hosted controller returns these at the top level instead.
	IPAssignments []string `json:"ipAssignments"`
	Authorized    *bool    `json:"authorized"`
	Address       string   `json:"address"`
}

func (z *ZeroTier) get(ctx context.Context, path string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(z.API, "/")+path, nil)
	if err != nil {
		return err
	}
	if z.Token != "" {
		req.Header.Set("Authorization", "token "+z.Token)
		req.Header.Set("X-ZT1-Auth", z.Token)
	}
	resp, err := z.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return constErr("zerotier: " + resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(into)
}

// fetchMembers reads the network's members from whichever controller is
// configured.
//
// Central answers /network/{id}/member with a LIST of members. A self-hosted
// controller answers the same path with an OBJECT of memberId to revision,
// and each member has to be fetched separately. Both are handled because
// "self-hosted or not" should not be a different feature.
func (z *ZeroTier) fetchMembers(ctx context.Context) (map[netip.Addr]Identity, error) {
	base := "/network/" + z.Network + "/member"
	var raw json.RawMessage
	if err := z.get(ctx, base, &raw); err != nil {
		// A self-hosted controller puts the same API under /controller.
		if err2 := z.get(ctx, "/controller"+base, &raw); err2 != nil {
			return nil, err
		}
		base = "/controller" + base
	}

	var list []ztMember
	if err := json.Unmarshal(raw, &list); err != nil {
		var index map[string]any
		if err := json.Unmarshal(raw, &index); err != nil {
			return nil, err
		}
		for id := range index {
			var m ztMember
			if err := z.get(ctx, base+"/"+id, &m); err != nil {
				continue // one unreadable member must not lose the rest
			}
			list = append(list, m)
		}
	}

	out := map[netip.Addr]Identity{}
	for _, m := range list {
		ips := m.Config.IPAssignments
		if len(ips) == 0 {
			ips = m.IPAssignments
		}
		authorized := m.Config.Authorized
		if m.Authorized != nil {
			authorized = *m.Authorized
		}
		// An unauthorized member is one the operator has not admitted to the
		// network. It may still hold a stale assignment, and treating that as
		// an identity would name somebody who is currently denied access.
		if !authorized {
			continue
		}
		node := m.NodeID
		if node == "" {
			node = m.Address
		}
		if node == "" {
			continue
		}
		name := strings.TrimSpace(m.Name)
		for _, s := range ips {
			addr, err := netip.ParseAddr(strings.TrimSpace(s))
			if err != nil {
				continue
			}
			addr = addr.Unmap()
			if len(z.Ranges) > 0 && !InAny(addr, z.Ranges) {
				continue
			}
			out[addr] = Identity{
				Subject:  "zerotier:" + node,
				Device:   name,
				Backend:  "zerotier",
				Verified: true,
			}
		}
	}
	return out, nil
}
