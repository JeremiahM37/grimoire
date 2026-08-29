package identity

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

// The property everything else rests on: identity is decided from the address
// the TCP stack accepted, never from something the caller sent. Every backend
// here is address-based, so a peer read out of a header would let any caller
// claim to be any node on the overlay.

func req(remote string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/notes", nil)
	r.RemoteAddr = remote
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestPeerAddrReadsTheConnectionNotTheHeaders(t *testing.T) {
	r := req("100.64.1.9:51820", map[string]string{
		"X-Forwarded-For": "100.64.9.9",
		"X-Real-IP":       "100.64.9.9",
		"Forwarded":       "for=100.64.9.9",
	})
	got, ok := PeerAddr(r)
	if !ok {
		t.Fatal("no peer address")
	}
	if got.String() != "100.64.1.9" {
		t.Fatalf("peer = %s, want 100.64.1.9 — a forwarded header must never "+
			"decide identity, or anyone can claim any node", got)
	}
}

func TestPeerAddrHandlesTheAddressFormsAListenerProduces(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"127.0.0.1:8080", "127.0.0.1"},
		{"[fd7a:115c:a1e0::1]:443", "fd7a:115c:a1e0::1"},
		{"[::ffff:100.64.0.5]:9", "100.64.0.5"}, // v4-mapped must compare as v4
		{"100.64.0.5", "100.64.0.5"},            // no port
	} {
		got, ok := PeerAddr(req(tc.remote, nil))
		if !ok || got.String() != tc.want {
			t.Errorf("PeerAddr(%q) = %v/%v, want %s", tc.remote, got, ok, tc.want)
		}
	}
	if _, ok := PeerAddr(req("not-an-address", nil)); ok {
		t.Error("an unparseable peer must not resolve to an identity")
	}
}

func TestNoBackendsIdentifiesNobody(t *testing.T) {
	r := New()
	if r.Enabled() {
		t.Error("a resolver with no backends reports itself enabled")
	}
	if _, ok := r.Identify(req("100.64.0.1:1", nil)); ok {
		t.Error("identity was resolved with nothing configured; the default " +
			"deployment must behave exactly as it did before")
	}
}

// stub is a backend that answers for one address.
type stub struct {
	name string
	addr string
	id   Identity
}

func (s stub) Name() string { return s.name }
func (s stub) Identify(peer netip.Addr, _ *http.Request) (Identity, bool) {
	if peer.String() == s.addr {
		return s.id, true
	}
	return Identity{}, false
}

func TestFirstConfiguredBackendWins(t *testing.T) {
	a := stub{name: "a", addr: "100.64.0.1", id: Identity{Subject: "a:one"}}
	b := stub{name: "b", addr: "100.64.0.1", id: Identity{Subject: "b:one"}}
	got, ok := New(a, b).Identify(req("100.64.0.1:1", nil))
	if !ok || got.Subject != "a:one" {
		t.Fatalf("got %+v, want the first configured backend to answer", got)
	}
	if got.Backend != "a" {
		t.Errorf("backend = %q; the result must name which mechanism answered", got.Backend)
	}
}

func TestABackendThatPassesFallsThroughToTheNext(t *testing.T) {
	a := stub{name: "a", addr: "10.0.0.1", id: Identity{Subject: "a:one"}}
	b := stub{name: "b", addr: "100.64.0.1", id: Identity{Subject: "b:one"}}
	got, ok := New(a, b).Identify(req("100.64.0.1:1", nil))
	if !ok || got.Subject != "b:one" {
		t.Fatalf("got %+v/%v, want b to answer after a passed", got, ok)
	}
}

// A backend returning ok with nothing in it would produce a bare "tailscale:"
// subject that collides with every other empty answer.
func TestAnEmptySubjectIsNotAnIdentity(t *testing.T) {
	empty := stub{name: "e", addr: "100.64.0.1", id: Identity{}}
	if _, ok := New(empty).Identify(req("100.64.0.1:1", nil)); ok {
		t.Error("a backend answering with no subject must not resolve")
	}
}

func TestNamesReportsWhatIsRunning(t *testing.T) {
	r := New(stub{name: "tailscale"}, stub{name: "proxy"})
	got := r.Names()
	if len(got) != 2 || got[0] != "tailscale" || got[1] != "proxy" {
		t.Fatalf("names = %v, want configured order preserved", got)
	}
}

func TestParsePrefixesTakesCIDRsAndBareAddresses(t *testing.T) {
	got := ParsePrefixes("127.0.0.1, 10.0.0.0/8 , ::1,  , garbage, 100.64.0.0/10")
	if len(got) != 4 {
		t.Fatalf("parsed %d prefixes from the list, want 4: %v", len(got), got)
	}
	// A bare address must bound to exactly that host, not to its whole class.
	if !got[0].Contains(netip.MustParseAddr("127.0.0.1")) ||
		got[0].Contains(netip.MustParseAddr("127.0.0.2")) {
		t.Errorf("bare address became %v, want a single host", got[0])
	}
}

func TestInAnyIsExact(t *testing.T) {
	nets := ParsePrefixes("100.64.0.0/10")
	if !InAny(netip.MustParseAddr("100.64.0.1"), nets) {
		t.Error("an address inside the range was rejected")
	}
	if InAny(netip.MustParseAddr("100.128.0.1"), nets) {
		t.Error("an address outside the range was accepted; the range check is " +
			"what establishes that a packet arrived over the overlay")
	}
	if InAny(netip.MustParseAddr("100.64.0.1"), nil) {
		t.Error("no ranges must mean no match, not match-everything")
	}
}
