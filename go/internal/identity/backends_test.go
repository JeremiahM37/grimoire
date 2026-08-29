package identity

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// ------------------------------------------------------------- tailscale

// tailscaledStub answers LocalAPI whois the way the daemon does.
func tailscaledStub(t *testing.T, byAddr map[string]whoisReply) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/localapi/v0/whois" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		ap, err := netip.ParseAddrPort(r.URL.Query().Get("addr"))
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		reply, ok := byAddr[ap.Addr().String()]
		if !ok {
			// What tailscaled returns for an address it does not know.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(reply)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func laptop() whoisReply {
	var r whoisReply
	r.Node.Name = "laptop.tail878d9e.ts.net."
	r.Node.Hostinfo.Hostname = "laptop"
	r.Node.Hostinfo.OS = "linux"
	r.UserProfile.LoginName = "jam@github"
	r.UserProfile.DisplayName = "Jam"
	return r
}

func TestTailscaleNamesTheNodeAndTheUser(t *testing.T) {
	srv, _ := tailscaledStub(t, map[string]whoisReply{"100.64.0.9": laptop()})
	ts := &Tailscale{Endpoint: srv.URL}

	id, ok := ts.Identify(netip.MustParseAddr("100.64.0.9"), req("100.64.0.9:1", nil))
	if !ok {
		t.Fatal("a known tailnet peer was not identified")
	}
	if id.Subject != "tailscale:jam@github" {
		t.Errorf("subject = %q, want tailscale:jam@github", id.Subject)
	}
	if id.Device != "laptop" || id.User != "jam@github" {
		t.Errorf("device/user = %q/%q, want laptop/jam@github", id.Device, id.User)
	}
	if !id.Verified {
		t.Error("a tailnet address is reachable only through the tunnel; the " +
			"answer is authenticated and must say so")
	}
}

// The range check is the security boundary: without it, a lookup of any
// address the daemon happens to answer for becomes an identity.
func TestTailscaleIgnoresPeersOutsideTheTailnetRange(t *testing.T) {
	srv, hits := tailscaledStub(t, map[string]whoisReply{"192.168.0.50": laptop()})
	ts := &Tailscale{Endpoint: srv.URL}

	if _, ok := ts.Identify(netip.MustParseAddr("192.168.0.50"), req("192.168.0.50:1", nil)); ok {
		t.Error("a LAN address was identified as a tailnet node")
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("the daemon was queried %d times about an off-tailnet address; "+
			"the range check must come first", n)
	}
}

func TestTailscaleReturnsNothingForAnUnknownPeer(t *testing.T) {
	srv, _ := tailscaledStub(t, map[string]whoisReply{})
	ts := &Tailscale{Endpoint: srv.URL}
	if _, ok := ts.Identify(netip.MustParseAddr("100.64.0.9"), req("100.64.0.9:1", nil)); ok {
		t.Error("an address the daemon does not know must not become an identity")
	}
}

func TestTailscaleSurvivesADeadDaemon(t *testing.T) {
	// Nothing is listening here, which is what an unconfigured or stopped
	// tailscaled looks like. It must degrade to "unidentified", not to an error
	// that fails the request the caller actually made.
	ts := &Tailscale{Endpoint: "http://127.0.0.1:1"}
	if _, ok := ts.Identify(netip.MustParseAddr("100.64.0.9"), req("100.64.0.9:1", nil)); ok {
		t.Error("a dead daemon produced an identity")
	}
}

func TestTailscaleCachesSoIdentityIsNotASocketRoundTripPerRequest(t *testing.T) {
	srv, hits := tailscaledStub(t, map[string]whoisReply{"100.64.0.9": laptop()})
	ts := &Tailscale{Endpoint: srv.URL, TTL: time.Minute}
	for i := 0; i < 25; i++ {
		if _, ok := ts.Identify(netip.MustParseAddr("100.64.0.9"), req("100.64.0.9:1", nil)); !ok {
			t.Fatal("lookup failed mid-run")
		}
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("queried the daemon %d times for 25 requests, want 1", n)
	}
}

func TestTailscaleFallsBackToTheMagicDNSNameWhenThereIsNoHostname(t *testing.T) {
	var r whoisReply
	r.Node.Name = "buildbox.tail878d9e.ts.net."
	r.UserProfile.LoginName = "ci@example.com"
	srv, _ := tailscaledStub(t, map[string]whoisReply{"100.64.0.4": r})
	ts := &Tailscale{Endpoint: srv.URL}
	id, ok := ts.Identify(netip.MustParseAddr("100.64.0.4"), req("100.64.0.4:1", nil))
	if !ok || id.Device != "buildbox" {
		t.Fatalf("device = %q (ok=%v), want buildbox from the MagicDNS name", id.Device, ok)
	}
}

// ------------------------------------------------------------- zerotier

// centralStub answers the way ZeroTier Central does: a list of members.
func centralStub(t *testing.T, members []map[string]any) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/network/8bd5124fd6a1b7cc/member" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(members)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func member(node, name, ip string, authorized bool) map[string]any {
	return map[string]any{
		"nodeId": node, "name": name,
		"config": map[string]any{
			"ipAssignments": []string{ip}, "authorized": authorized,
		},
	}
}

func TestZeroTierMapsAManagedAddressToItsMember(t *testing.T) {
	srv, _ := centralStub(t, []map[string]any{
		member("8bd5124fd6", "workstation", "10.147.17.42", true),
		member("a09acf0233", "phone", "10.147.17.99", true),
	})
	z := &ZeroTier{API: srv.URL, Network: "8bd5124fd6a1b7cc"}

	id, ok := z.Identify(netip.MustParseAddr("10.147.17.42"), req("10.147.17.42:1", nil))
	if !ok {
		t.Fatal("a member's managed address was not identified")
	}
	if id.Subject != "zerotier:8bd5124fd6" || id.Device != "workstation" {
		t.Errorf("got %+v, want the node id as subject and its name as device", id)
	}
}

// A member the operator has not admitted may still hold a stale assignment.
// Naming them would attribute writes to somebody currently denied access.
func TestZeroTierIgnoresUnauthorizedMembers(t *testing.T) {
	srv, _ := centralStub(t, []map[string]any{
		member("deadbeef01", "revoked-laptop", "10.147.17.50", false),
	})
	z := &ZeroTier{API: srv.URL, Network: "8bd5124fd6a1b7cc"}
	if _, ok := z.Identify(netip.MustParseAddr("10.147.17.50"), req("10.147.17.50:1", nil)); ok {
		t.Error("an unauthorized member was given an identity")
	}
}

func TestZeroTierHonoursTheConfiguredRange(t *testing.T) {
	srv, hits := centralStub(t, []map[string]any{
		member("8bd5124fd6", "workstation", "192.168.0.42", true),
	})
	z := &ZeroTier{API: srv.URL, Network: "8bd5124fd6a1b7cc",
		Ranges: ParsePrefixes("10.147.17.0/24")}
	if _, ok := z.Identify(netip.MustParseAddr("192.168.0.42"), req("192.168.0.42:1", nil)); ok {
		t.Error("an address outside the network's pool was identified")
	}
	if n := atomic.LoadInt32(hits); n != 0 {
		t.Errorf("fetched the member list %d times for an out-of-range peer, want 0", n)
	}
}

func TestZeroTierReadsASelfHostedControllerToo(t *testing.T) {
	// A local controller answers the member path with an object of id to
	// revision, and each member has to be fetched on its own.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/controller/network/8bd5124fd6a1b7cc/member":
			_ = json.NewEncoder(w).Encode(map[string]int{"8bd5124fd6": 3})
		case "/controller/network/8bd5124fd6a1b7cc/member/8bd5124fd6":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"address": "8bd5124fd6", "name": "selfhosted-node",
				"ipAssignments": []string{"10.147.17.7"}, "authorized": true,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	z := &ZeroTier{API: srv.URL, Network: "8bd5124fd6a1b7cc"}
	id, ok := z.Identify(netip.MustParseAddr("10.147.17.7"), req("10.147.17.7:1", nil))
	if !ok || id.Subject != "zerotier:8bd5124fd6" {
		t.Fatalf("got %+v/%v, want the self-hosted controller shape to work", id, ok)
	}
	if id.Device != "selfhosted-node" {
		t.Errorf("device = %q, want selfhosted-node", id.Device)
	}
}

// A controller that goes down should cost freshness, not attribution.
func TestZeroTierKeepsServingTheLastMapWhenTheControllerFails(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			member("8bd5124fd6", "workstation", "10.147.17.42", true)})
	}))
	defer srv.Close()

	z := &ZeroTier{API: srv.URL, Network: "8bd5124fd6a1b7cc", TTL: time.Nanosecond}
	if _, ok := z.Identify(netip.MustParseAddr("10.147.17.42"), req("10.147.17.42:1", nil)); !ok {
		t.Fatal("first lookup failed")
	}
	up.Store(false)
	if _, ok := z.Identify(netip.MustParseAddr("10.147.17.42"), req("10.147.17.42:1", nil)); !ok {
		t.Error("a briefly unreachable controller dropped every caller's identity; " +
			"stale attribution beats a burst of unattributed writes")
	}
}

func TestZeroTierWithoutANetworkIsInert(t *testing.T) {
	z := &ZeroTier{API: "http://127.0.0.1:1"}
	if _, ok := z.Identify(netip.MustParseAddr("10.147.17.42"), req("10.147.17.42:1", nil)); ok {
		t.Error("a backend with no network to look in produced an identity")
	}
}

func TestZeroTierSendsBothControllerFlavoursOfAuth(t *testing.T) {
	var sawCentral, sawLocal bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawCentral = r.Header.Get("Authorization") == "token s3cret"
		sawLocal = r.Header.Get("X-ZT1-Auth") == "s3cret"
		_ = json.NewEncoder(w).Encode([]map[string]any{})
	}))
	defer srv.Close()
	z := &ZeroTier{API: srv.URL, Network: "n", Token: "s3cret"}
	z.Identify(netip.MustParseAddr("10.147.17.42"), req("10.147.17.42:1", nil))
	if !sawCentral || !sawLocal {
		t.Errorf("central=%v local=%v; both header styles are sent so that "+
			"self-hosted and Central are not different features", sawCentral, sawLocal)
	}
}

// ------------------------------------------------------------- proxy

func TestProxyTrustsTheHeaderOnlyFromATrustedAddress(t *testing.T) {
	p := &Proxy{From: ParsePrefixes("127.0.0.1")}

	id, ok := p.Identify(netip.MustParseAddr("127.0.0.1"),
		req("127.0.0.1:9", map[string]string{"Remote-User": "jam"}))
	if !ok || id.Subject != "proxy:jam" || id.User != "jam" {
		t.Fatalf("got %+v/%v, want proxy:jam from the trusted proxy", id, ok)
	}

	// The whole point. Anything else setting the header is a forgery.
	if _, ok := p.Identify(netip.MustParseAddr("192.168.0.66"),
		req("192.168.0.66:9", map[string]string{"Remote-User": "admin"})); ok {
		t.Error("a header from an untrusted address was accepted; that is the " +
			"forgery this backend exists to prevent")
	}
}

func TestProxyWithNothingTrustedIsInert(t *testing.T) {
	p := &Proxy{}
	if _, ok := p.Identify(netip.MustParseAddr("127.0.0.1"),
		req("127.0.0.1:9", map[string]string{"Remote-User": "jam"})); ok {
		t.Error("a proxy backend with no trusted addresses read the header anyway; " +
			"being inert is the only safe way to be misconfigured")
	}
}

func TestProxyReadsAConfiguredHeaderAndDevice(t *testing.T) {
	p := &Proxy{From: ParsePrefixes("10.0.0.0/8"),
		Header: "X-Auth-User", DeviceHeader: "X-Auth-Device"}
	id, ok := p.Identify(netip.MustParseAddr("10.1.2.3"),
		req("10.1.2.3:9", map[string]string{"X-Auth-User": "jam", "X-Auth-Device": "kiosk"}))
	if !ok || id.User != "jam" || id.Device != "kiosk" {
		t.Fatalf("got %+v/%v", id, ok)
	}
}

func TestProxyWithNoUserHeaderDoesNotIdentify(t *testing.T) {
	p := &Proxy{From: ParsePrefixes("127.0.0.1")}
	if _, ok := p.Identify(netip.MustParseAddr("127.0.0.1"), req("127.0.0.1:9", nil)); ok {
		t.Error("an absent header must mean unidentified, not an empty user")
	}
}

// ------------------------------------------------------------- resolver end to end

func TestResolverPrefersTheOverlayOverTheProxyHeader(t *testing.T) {
	srv, _ := tailscaledStub(t, map[string]whoisReply{"100.64.0.9": laptop()})
	r := New(&Tailscale{Endpoint: srv.URL}, &Proxy{From: ParsePrefixes("100.64.0.0/10")})

	// Both could answer. The tailnet answer is authenticated by the tunnel;
	// the header is only as good as the address it came from, which here is
	// not a proxy at all.
	got, ok := r.Identify(req("100.64.0.9:1", map[string]string{"Remote-User": "somebody-else"}))
	if !ok {
		t.Fatal("nothing identified the caller")
	}
	if got.Subject != "tailscale:jam@github" {
		t.Errorf("subject = %q; a self-set header outranked the overlay", got.Subject)
	}
}

func TestConfigDoesNothingUnlessAsked(t *testing.T) {
	t.Setenv("GRIMOIRE_IDENTITY", "")
	if FromEnv().Enabled() {
		t.Error("identity resolution turned itself on")
	}
	t.Setenv("GRIMOIRE_IDENTITY", "off")
	if FromEnv().Enabled() {
		t.Error(`"off" enabled it`)
	}
}

func TestConfigBuildsOnlyBackendsThatCanAnswer(t *testing.T) {
	t.Setenv("GRIMOIRE_IDENTITY", "tailscale,zerotier,proxy,mtls,nonsense")
	// zerotier has no network and proxy trusts nobody, so neither can ever
	// answer and listing them as running would be a lie to the operator.
	t.Setenv("GRIMOIRE_ZEROTIER_NETWORK", "")
	t.Setenv("GRIMOIRE_IDENTITY_PROXY_FROM", "")
	got := FromEnv().Names()
	want := []string{"tailscale", "mtls"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("backends = %v, want %v", got, want)
	}
}

func TestConfigKeepsTheOrderTheOperatorWrote(t *testing.T) {
	t.Setenv("GRIMOIRE_IDENTITY", "proxy,tailscale")
	t.Setenv("GRIMOIRE_IDENTITY_PROXY_FROM", "127.0.0.1")
	got := FromEnv().Names()
	if fmt.Sprint(got) != fmt.Sprint([]string{"proxy", "tailscale"}) {
		t.Errorf("backends = %v, want the configured order", got)
	}
}

func TestHeadscaleIsServedByTheTailscaleBackend(t *testing.T) {
	t.Setenv("GRIMOIRE_IDENTITY", "headscale")
	if got := FromEnv().Names(); fmt.Sprint(got) != fmt.Sprint([]string{"tailscale"}) {
		t.Errorf("backends = %v; headscale uses the same client and LocalAPI", got)
	}
}

func TestTokenCanComeFromAFileRatherThanTheProcessTable(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/tok"
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRIMOIRE_ZEROTIER_TOKEN", "from-env")
	t.Setenv("GRIMOIRE_ZEROTIER_TOKEN_FILE", path)
	if got := envSecret("GRIMOIRE_ZEROTIER_TOKEN", "GRIMOIRE_ZEROTIER_TOKEN_FILE"); got != "from-file" {
		t.Errorf("token = %q, want the file to win and to be trimmed", got)
	}
}
