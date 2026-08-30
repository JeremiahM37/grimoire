package secrets

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

// A TTL bounds a grant in time and not at all in volume: fifteen minutes is
// fifteen minutes in which an agent may make any number of calls with a
// credential it cannot read. For "post this one webhook", the honest bound is
// one.

func limitedSetup(t *testing.T) (*Vault, *Broker, *httptest.Server, *int32) {
	t.Helper()
	v, b := testVault(t)
	if err := v.Initialize(testPassphrase); err != nil {
		t.Fatal(err)
	}
	if err := v.PutVersioned("api", "the-value", nil, ""); err != nil {
		t.Fatal(err)
	}
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return v, b, srv, &hits
}

func TestASingleUseGrantWorksExactlyOnce(t *testing.T) {
	_, b, srv, hits := limitedSetup(t)
	token, err := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Use(token, "GET", srv.URL, "Authorization", ""); err != nil {
		t.Fatalf("first use failed: %v", err)
	}
	if _, err := b.Use(token, "GET", srv.URL, "Authorization", ""); err == nil {
		t.Fatal("a single-use grant was redeemed twice")
	}
	if n := atomic.LoadInt32(hits); n != 1 {
		t.Errorf("the far side saw %d requests, want 1 — a refused redemption "+
			"must not still make the call", n)
	}
}

// The case the whole design turns on. A read-then-write would let two callers
// both observe a one-shot grant as unused, and it only shows up under load.
func TestConcurrentRedemptionsCannotExceedTheLimit(t *testing.T) {
	_, b, srv, hits := limitedSetup(t)
	const limit = 3
	token, err := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: limit})
	if err != nil {
		t.Fatal(err)
	}
	var ok, refused int32
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Use(token, "GET", srv.URL, "Authorization", ""); err != nil {
				atomic.AddInt32(&refused, 1)
			} else {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if ok != limit {
		t.Errorf("%d redemptions succeeded, want exactly %d", ok, limit)
	}
	if refused != 40-limit {
		t.Errorf("%d refused, want %d", refused, 40-limit)
	}
	if n := atomic.LoadInt32(hits); n != limit {
		t.Errorf("the far side saw %d requests, want %d", n, limit)
	}
}

// A spent grant left in the list reads as usable.
func TestASpentGrantStopsAppearing(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: 1})
	b.Use(token, "GET", srv.URL, "Authorization", "")

	grants, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range grants {
		if g.Token == token {
			t.Error("a fully spent grant is still listed as though it could be used")
		}
	}
}

func TestAnUnlimitedGrantIsStillUnlimited(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	// MaxUses 0 is what every grant issued before this existed was, and it must
	// keep meaning "bounded only by the TTL".
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300})
	for i := 0; i < 5; i++ {
		if _, err := b.Use(token, "GET", srv.URL, "Authorization", ""); err != nil {
			t.Fatalf("use %d of an unlimited grant failed: %v", i+1, err)
		}
	}
	grants, _ := b.List()
	found := false
	for _, g := range grants {
		if g.Token == token {
			found = true
			if g.Uses != 5 {
				t.Errorf("uses = %d, want 5 — an unlimited grant still counts", g.Uses)
			}
			if g.MaxUses != 0 {
				t.Errorf("max_uses = %d, want 0", g.MaxUses)
			}
		}
	}
	if !found {
		t.Error("an unlimited grant was retired")
	}
}

func TestRemainingUsesAreVisibleBeforeTheyRunOut(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: 3})
	b.Use(token, "GET", srv.URL, "Authorization", "")

	grants, _ := b.List()
	for _, g := range grants {
		if g.Token == token {
			if g.Uses != 1 || g.MaxUses != 3 {
				t.Errorf("uses/max = %d/%d, want 1/3 — the console cannot show "+
					"what is left without both", g.Uses, g.MaxUses)
			}
		}
	}
}

func TestANegativeLimitIsRefusedRatherThanTreatedAsUnlimited(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	if _, err := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: -1}); err == nil {
		t.Error("a negative limit was accepted; it would silently mean unlimited")
	}
}

// An exhausted redemption is a denial and belongs in the trail with the others.
func TestAnExhaustedGrantIsAudited(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: 1})
	b.Use(token, "GET", srv.URL, "Authorization", "")
	b.Use(token, "GET", srv.URL, "Authorization", "")

	entries, err := b.Audit(50)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e["action"] == "denied" {
			if d, _ := e["detail"].(string); d != "" {
				return // found it
			}
		}
	}
	t.Error("a refused redemption left no audit entry")
}

// An agent that spent its grant and an agent holding a bad token want opposite
// responses: ask for another, versus fix your bug. They must not get the same
// sentence.
func TestSpendingAGrantSaysSoRatherThanLookingLikeARevocation(t *testing.T) {
	_, b, srv, _ := limitedSetup(t)
	token, _ := b.Grant(GrantSpec{Secret: "api", Grantee: "agent",
		Scope: srv.URL, TTLSeconds: 300, MaxUses: 1})
	b.Use(token, "GET", srv.URL, "Authorization", "")

	_, err := b.Use(token, "GET", srv.URL, "Authorization", "")
	if err == nil {
		t.Fatal("a spent grant was redeemed")
	}
	if got := err.Error(); got != "grant has no uses left" {
		t.Errorf("error = %q, want it to name exhaustion; an agent cannot tell "+
			"a spent grant from a wrong token if both say the same thing", got)
	}
	// And a genuinely unknown token still says that.
	if _, err := b.Use("not-a-token", "GET", srv.URL, "Authorization", ""); err == nil ||
		err.Error() != "unknown or revoked grant" {
		t.Errorf("unknown token error = %v", err)
	}
}
