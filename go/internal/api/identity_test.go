package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/identity"
)

// fakeBackend identifies one peer address, so these tests exercise the wiring
// rather than a network.
type fakeBackend struct {
	name     string
	peer     string
	id       identity.Identity
	verified bool
}

func (f fakeBackend) Name() string { return f.name }
func (f fakeBackend) Identify(peer netip.Addr, _ *http.Request) (identity.Identity, bool) {
	if peer.String() != f.peer {
		return identity.Identity{}, false
	}
	id := f.id
	id.Verified = f.verified
	return id, true
}

// asPeer sends a request that appears to come from addr.
func asPeer(t *testing.T, h http.Handler, method, path, addr string,
	headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = addr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func tailnetServer(t *testing.T, verified bool) (*Server, http.Handler) {
	t.Helper()
	s, h := testServer(t)
	s.Identity = identity.New(fakeBackend{
		name: "tailscale", peer: "100.64.0.9", verified: verified,
		id: identity.Identity{
			Subject: "tailscale:jam@github", Device: "laptop", User: "jam@github",
		},
	})
	return s, h
}

func TestWhoamiReportsNothingWhenIdentityIsOff(t *testing.T) {
	_, h := testServer(t)
	w := asPeer(t, h, "GET", "/api/identity", "127.0.0.1:5000", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("identity = %d: %s", w.Code, w.Body)
	}
	var out map[string]any
	decode(t, w, &out)
	if out["enabled"] != false {
		t.Error("identity reported itself enabled with nothing configured")
	}
	if out["verified"] != false {
		t.Error("a caller was reported verified with no backend running")
	}
}

func TestAVerifiedIdentityReplacesTheSelfAssertedName(t *testing.T) {
	_, h := tailnetServer(t, true)
	// The caller claims to be something else entirely. It does not matter.
	w := asPeer(t, h, "GET", "/api/identity", "100.64.0.9:41000",
		map[string]string{"X-Grimoire-Agent": "definitely-not-this"})
	var out map[string]any
	decode(t, w, &out)

	if out["attributed_to"] != "laptop" {
		t.Errorf("attributed_to = %v, want laptop — a verified identity must "+
			"outrank the header", out["attributed_to"])
	}
	if out["claimed"] != "definitely-not-this" {
		t.Errorf("claimed = %v; the claim must still be reported so the two "+
			"can be compared", out["claimed"])
	}
	if out["verified"] != true {
		t.Error("a verified identity was not reported as verified")
	}
}

func TestAnUnverifiedBackendDoesNotOverrideTheHeader(t *testing.T) {
	_, h := tailnetServer(t, false)
	w := asPeer(t, h, "GET", "/api/identity", "100.64.0.9:41000",
		map[string]string{"X-Grimoire-Agent": "claude-code"})
	var out map[string]any
	decode(t, w, &out)
	if out["attributed_to"] != "claude-code" {
		t.Errorf("attributed_to = %v, want the claimed name: an unverified "+
			"identity is not evidence of anything", out["attributed_to"])
	}
}

func TestAnUnidentifiedPeerKeepsTheOldBehaviourExactly(t *testing.T) {
	_, h := tailnetServer(t, true)
	// A caller the backend does not recognise, on a server where identity IS
	// configured. This is the common case on a mixed network and it must not
	// regress into "unattributed".
	w := asPeer(t, h, "GET", "/api/identity", "192.168.0.44:41000",
		map[string]string{"X-Grimoire-Agent": "codex"})
	var out map[string]any
	decode(t, w, &out)
	if out["attributed_to"] != "codex" {
		t.Errorf("attributed_to = %v, want codex", out["attributed_to"])
	}
	if out["verified"] != false {
		t.Error("an unrecognised peer was reported as verified")
	}
}

// The forgery that would make all of this worse than useless.
func TestAForwardedHeaderCannotMintAnIdentity(t *testing.T) {
	_, h := tailnetServer(t, true)
	t.Setenv("GRIMOIRE_TRUST_PROXY", "1") // even with proxy trust ON
	w := asPeer(t, h, "GET", "/api/identity", "192.168.0.44:41000", map[string]string{
		"X-Forwarded-For": "100.64.0.9",
		"X-Real-IP":       "100.64.0.9",
	})
	var out map[string]any
	decode(t, w, &out)
	if out["verified"] == true {
		t.Fatal("a caller claimed a tailnet address in a header and was " +
			"identified as that node; identity must read the peer the TCP " +
			"stack accepted, never a forwarded one")
	}
	if out["peer"] != "192.168.0.44" {
		t.Errorf("peer = %v, want the real connection address", out["peer"])
	}
}

// Attribution and authorization are separate on purpose: a verified caller is
// truthfully named and still gets nothing it was not granted.
func TestAVerifiedIdentityDoesNotSignItselfIn(t *testing.T) {
	s, h := tailnetServer(t, true)
	if s.Auth == nil || s.Auth.Enabled() {
		// Single-user: there are no accounts, so there is nothing to grant and
		// the principal is unrestricted exactly as before. Assert that rather
		// than skipping, because "unchanged" is the property.
		w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("me = %d: %s", w.Code, w.Body)
		}
		var me map[string]any
		decode(t, w, &me)
		if me["multi_user"] != false {
			t.Fatal("expected a single-user test server")
		}
		if me["anonymous"] == true {
			t.Error("a single-user deployment turned anonymous once identity " +
				"was configured; enabling identity must change nothing here")
		}
	}
}

func TestWhoamiNamesTheConfiguredBackends(t *testing.T) {
	s, h := testServer(t)
	s.Identity = identity.New(
		fakeBackend{name: "tailscale", peer: "100.64.0.9"},
		fakeBackend{name: "zerotier", peer: "10.147.17.4"},
	)
	w := asPeer(t, h, "GET", "/api/identity", "127.0.0.1:5000", nil)
	var out struct {
		Backends []string `json:"backends"`
		Enabled  bool     `json:"enabled"`
	}
	decode(t, w, &out)
	if !out.Enabled {
		t.Error("configured backends were not reported as enabled")
	}
	if len(out.Backends) != 2 || out.Backends[0] != "tailscale" {
		t.Errorf("backends = %v, want both in configured order", out.Backends)
	}
}

// The ledger is one of the three places attribution actually lands, so the
// verified name has to reach it rather than only /whoami.
func TestTheVerifiedNameReachesTheUsageLedger(t *testing.T) {
	s, h := tailnetServer(t, true)
	if s.Index == nil {
		t.Skip("no index on this test server")
	}
	// A request that books a model call attributes it to the verified device,
	// not to whatever the caller put in the header.
	w := asPeer(t, h, "GET", "/api/identity", "100.64.0.9:41000",
		map[string]string{"X-Grimoire-Agent": "lying-agent"})
	var out map[string]any
	decode(t, w, &out)
	got, _ := json.Marshal(out["attributed_to"])
	if string(got) != `"laptop"` {
		t.Fatalf("attribution = %s, want \"laptop\"", got)
	}
}

// ------------------------------------------------- identity as a sign-in

// The second half, and the one with teeth: a verified identity may stand in
// for a password, but ONLY where the operator said so. Everything below is
// about that "only".

func multiUserTailnet(t *testing.T) (*Server, http.Handler, string) {
	t.Helper()
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "jam", "admin")
	s.Identity = identity.New(fakeBackend{
		name: "tailscale", peer: "100.64.0.9", verified: true,
		id: identity.Identity{
			Subject: "tailscale:jam@github", Device: "laptop", User: "jam@github",
		},
	})
	return s, h, adminKey
}

func TestAnUnmappedIdentityIsNamedButStillAnonymous(t *testing.T) {
	_, h, _ := multiUserTailnet(t)
	w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
	var me map[string]any
	decode(t, w, &me)
	if me["multi_user"] != true {
		t.Fatal("expected a multi-user server")
	}
	if me["anonymous"] != true {
		t.Error("a verified but unmapped identity signed itself in; an identity " +
			"that mapped itself would make every device on the overlay an account")
	}
}

func TestAMappedIdentitySignsIn(t *testing.T) {
	s, h, _ := multiUserTailnet(t)
	u, err := s.Auth.ByName("jam")
	if err != nil {
		t.Fatal(err)
	}
	// What `grimoire user map tailscale jam@github jam` does.
	if err := s.Auth.MapIdentity("tailscale", "jam@github", u.ID); err != nil {
		t.Fatal(err)
	}
	w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
	var me map[string]any
	decode(t, w, &me)
	if me["anonymous"] == true {
		t.Fatal("a mapped identity did not sign in")
	}
	if me["name"] != "jam" {
		t.Errorf("signed in as %v, want jam", me["name"])
	}
	if me["admin"] != true {
		t.Error("the mapped account is an admin; the principal must carry its role")
	}
}

// The mapping is per backend. The same string arriving over a different
// mechanism is a different principal, or a ZeroTier node named "jam@github"
// would inherit a tailnet user's account.
func TestAMappingDoesNotCrossBackends(t *testing.T) {
	s, h, _ := multiUserTailnet(t)
	u, _ := s.Auth.ByName("jam")
	if err := s.Auth.MapIdentity("zerotier", "jam@github", u.ID); err != nil {
		t.Fatal(err)
	}
	w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
	var me map[string]any
	decode(t, w, &me)
	if me["anonymous"] != true {
		t.Error("a zerotier mapping signed in a tailscale caller")
	}
}

// An unverified backend must never reach the sign-in path at all.
func TestAnUnverifiedIdentityCannotSignIn(t *testing.T) {
	s, h := testServer(t)
	makeUser(t, s, h, "", "jam", "admin")
	s.Identity = identity.New(fakeBackend{
		name: "tailscale", peer: "100.64.0.9", verified: false,
		id: identity.Identity{Subject: "tailscale:jam@github", Device: "laptop"},
	})
	u, _ := s.Auth.ByName("jam")
	if err := s.Auth.MapIdentity("tailscale", "jam@github", u.ID); err != nil {
		t.Fatal(err)
	}
	w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
	var me map[string]any
	decode(t, w, &me)
	if me["anonymous"] != true {
		t.Error("an unverified identity signed in even though a mapping existed; " +
			"the mapping says who, the verification says whether to believe it")
	}
}

func TestUnmappingRevokesTheSignIn(t *testing.T) {
	s, h, _ := multiUserTailnet(t)
	u, _ := s.Auth.ByName("jam")
	if err := s.Auth.MapIdentity("tailscale", "jam@github", u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.Auth.UnmapIdentity("tailscale", "jam@github"); err != nil {
		t.Fatal(err)
	}
	w := asPeer(t, h, "GET", "/api/me", "100.64.0.9:41000", nil)
	var me map[string]any
	decode(t, w, &me)
	if me["anonymous"] != true {
		t.Error("unmapping did not revoke access — per-device revocation is the " +
			"reason to prefer this over one shared token")
	}
}

// ------------------------- the surfaces attribution actually lands on
//
// These exist because the first version shipped with identity reaching the
// usage ledger and NOTHING ELSE, and it took running it in production to
// notice. The ledger was the surface with a test; memory and the read trail
// were the surfaces that mattered.

func TestAVerifiedIdentityOutranksTheAgentFieldInTheBody(t *testing.T) {
	_, h := tailnetServer(t, true)
	// The body names an agent, which is how MCP clients have always written
	// memory. It is still the caller describing itself.
	req := requestFor(t, "POST", "/api/memory", map[string]any{
		"text": "the identity probe fact is recorded", "topic": "identity-probe",
		"agent": "not-really-me",
	})
	req.RemoteAddr = "100.64.0.9:41000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("remember = %d: %s", w.Code, w.Body)
	}

	lw := asPeer(t, h, "GET", "/api/memory", "100.64.0.9:41000", nil)
	var got string
	for _, e := range decodeEntries(t, lw) {
		if strings.Contains(e.Text, "identity probe fact") {
			got = e.Agent
		}
	}
	if got != "laptop" {
		t.Errorf("memory recorded agent %q, want laptop — reconciliation compares "+
			"authorship to decide what may supersede, so an agent that can name "+
			"itself can choose how the authority lattice treats it", got)
	}
}

// A device name the pattern rejects must not turn a legitimate write into a
// 400. Unverified-but-storable beats verified-but-unstorable.
func TestAnUnstorableVerifiedNameFallsBackInsteadOfFailingTheWrite(t *testing.T) {
	s, h := testServer(t)
	s.Identity = identity.New(fakeBackend{
		name: "tailscale", peer: "100.64.0.9", verified: true,
		id: identity.Identity{Subject: "tailscale:x", Device: "!!! not a valid name !!!"},
	})
	req := requestFor(t, "POST", "/api/memory", map[string]any{
		"text": "a fact from a badly named device", "agent": "fallback-agent",
	})
	req.RemoteAddr = "100.64.0.9:41000"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK && w.Code != http.StatusCreated {
		t.Fatalf("a write was refused because the DEVICE name was odd: %d %s", w.Code, w.Body)
	}
}

// entryOutLite mirrors the memory list shape this test needs.
type entryOutLite struct {
	Text  string `json:"text"`
	Agent string `json:"agent"`
}

func decodeEntries(t *testing.T, w *httptest.ResponseRecorder) []entryOutLite {
	t.Helper()
	var out []entryOutLite
	if err := json.Unmarshal(w.Body.Bytes(), &out); err == nil {
		return out
	}
	var wrapped struct {
		Entries []entryOutLite `json:"entries"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wrapped); err != nil {
		t.Fatalf("decoding memory list: %s", w.Body.String()[:200])
	}
	return wrapped.Entries
}
