package secrets

import (
	"strings"
	"testing"
)

// Just-in-time grants: an agent asks, a person answers.
//
// The property under test everywhere here is that ASKING gives nothing. Every
// other test in this file is a corollary of that one.

func readyVault(t *testing.T) (*Vault, *Broker) {
	t.Helper()
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	if err := v.Put("github-token", "ghp_secret_value", nil); err != nil {
		t.Fatal(err)
	}
	return v, b
}

func TestARequestGrantsNothing(t *testing.T) {
	v, b := readyVault(t)

	req, err := b.RequestGrant("github-token", "claude-4",
		"https://api.github.com/repos/acme/", "read the open issues on acme/thing", 600)
	if err != nil {
		t.Fatal(err)
	}
	if req.State != StatePending {
		t.Fatalf("state = %q, want pending", req.State)
	}
	if req.Token != "" {
		t.Fatal("asking returned a token — the request IS the grant, which is the " +
			"one thing this must never be")
	}
	grants, err := b.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(grants) != 0 {
		t.Fatalf("asking created %d grants", len(grants))
	}
	_ = v
}

func TestAskingWorksWhileTheVaultIsLocked(t *testing.T) {
	// The case this feature exists for: nobody is at the keyboard, which is
	// exactly when the vault is locked and when an agent needs to leave a
	// request rather than receive an error it cannot act on.
	v, b := readyVault(t)
	v.Lock()

	if _, err := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600); err != nil {
		t.Fatalf("asking with the vault locked failed: %v", err)
	}
	// …but approving does not, because approving mints a credential.
	reqs, _ := b.Requests(StatePending, 10)
	if len(reqs) != 1 {
		t.Fatalf("got %d pending", len(reqs))
	}
	if _, err := b.Approve(reqs[0].ID, "jam", 0); err != ErrLocked {
		t.Fatalf("approve with a locked vault = %v, want ErrLocked", err)
	}
}

func TestDenyingDoesNotNeedAnUnlockedVault(t *testing.T) {
	// Needing to unlock the credential store in order to REFUSE access to it
	// is backwards, and would mean the fast path for a suspicious request is
	// the one that requires the most ceremony.
	v, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600)
	v.Lock()

	out, err := b.Deny(req.ID, "jam", "use the read-only key")
	if err != nil {
		t.Fatalf("deny with a locked vault: %v", err)
	}
	if out.State != StateDenied || out.Note != "use the read-only key" {
		t.Errorf("denied request = %+v", out)
	}
}

func TestApprovalMintsAGrantTheAskerCanCollect(t *testing.T) {
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4",
		"https://api.github.com/", "reading issues", 600)

	approved, err := b.Approve(req.ID, "jam", 0)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Token == "" {
		t.Fatal("approval minted no token")
	}
	grants, _ := b.List()
	if len(grants) != 1 {
		t.Fatalf("got %d grants after approval", len(grants))
	}
	if grants[0].Scope != "https://api.github.com/" || grants[0].Grantee != "claude-4" {
		t.Errorf("grant = %+v — approval must issue what was ASKED for", grants[0])
	}

	// And the asker can collect it.
	got, err := b.Request(req.ID, "claude-4")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != approved.Token {
		t.Errorf("collected token %q != issued %q", got.Token, approved.Token)
	}
}

func TestOnlyTheAskerCanCollectTheToken(t *testing.T) {
	// This is the one read in the product that returns a live credential
	// token. If any caller could name a request id and receive it, the
	// approval step would be decorative.
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600)
	if _, err := b.Approve(req.ID, "jam", 0); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Request(req.ID, "some-other-agent"); err == nil {
		t.Fatal("another agent collected a token issued to claude-4")
	}
	if _, err := b.Request(req.ID, ""); err == nil {
		t.Fatal("a caller that named nobody collected a token")
	}
}

func TestListingRequestsNeverCarriesTokens(t *testing.T) {
	// The console polls this. A listing that carried live tokens would make
	// the approval UI the widest disclosure surface in the product.
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600)
	if _, err := b.Approve(req.ID, "jam", 0); err != nil {
		t.Fatal(err)
	}
	all, err := b.Requests("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d requests", len(all))
	}
	if all[0].Token != "" {
		t.Error("a request listing carried a live grant token")
	}
}

func TestADecisionCannotBeReplayed(t *testing.T) {
	// An approval that can be replayed is one that anybody who kept the id can
	// replay, minting a fresh credential per attempt.
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600)
	if _, err := b.Approve(req.ID, "jam", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Approve(req.ID, "jam", 0); err != ErrNoRequest {
		t.Fatalf("second approve = %v, want ErrNoRequest", err)
	}
	if _, err := b.Deny(req.ID, "jam", "changed my mind"); err != ErrNoRequest {
		t.Fatalf("deny after approve = %v, want ErrNoRequest", err)
	}
	grants, _ := b.List()
	if len(grants) != 1 {
		t.Errorf("replay minted %d grants", len(grants))
	}
}

func TestApproverCanShortenTheTTL(t *testing.T) {
	// Most of the point of a human in the loop: an agent that asks for a day
	// can be given ten minutes without anyone having to argue about it.
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 86400)
	approved, err := b.Approve(req.ID, "jam", 600)
	if err != nil {
		t.Fatal(err)
	}
	if approved.TTL != 600 {
		t.Errorf("ttl = %d, want the override", approved.TTL)
	}
	grants, _ := b.List()
	// 600s from now, not 86400 — compared loosely because Now() is real here.
	if len(grants) != 1 {
		t.Fatal("no grant")
	}
	if grants[0].ExpiresAt > float64(Now().Add(700*1e9).Unix()) {
		t.Errorf("the issued grant outlives the override: %+v", grants[0])
	}
}

func TestRepeatedIdenticalAsksCollapse(t *testing.T) {
	// An agent in a retry loop would otherwise post a hundred identical asks
	// and bury the one a person was about to approve.
	_, b := readyVault(t)
	first, _ := b.RequestGrant("github-token", "claude-4", "https://api.github.com/", "issues", 600)
	second, err := b.RequestGrant("github-token", "claude-4", "https://api.github.com/", "issues", 600)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID {
		t.Errorf("a repeated ask created a second request (%s vs %s)", second.ID, first.ID)
	}
	pending, _ := b.Requests(StatePending, 10)
	if len(pending) != 1 {
		t.Errorf("%d pending requests for one ask", len(pending))
	}
}

func TestADifferentScopeIsADifferentAsk(t *testing.T) {
	// Collapsing on scope too would silently merge "read one repo" with "read
	// everything", and a person approving the first would be granting the
	// second.
	_, b := readyVault(t)
	a, _ := b.RequestGrant("github-token", "claude-4", "https://api.github.com/repos/acme/", "x", 600)
	c, _ := b.RequestGrant("github-token", "claude-4", "https://api.github.com/", "x", 600)
	if a.ID == c.ID {
		t.Error("a broader scope was folded into a narrower pending request")
	}
}

func TestAskingForAnUnknownSecretIsNotAnOracle(t *testing.T) {
	// Refusing unknown names at ASK time would let anyone who can ask
	// enumerate which secrets the vault holds. The name is checked at
	// approval, where a person is already looking at it.
	_, b := readyVault(t)
	req, err := b.RequestGrant("does-not-exist", "claude-4", "", "trying it on", 600)
	if err != nil {
		t.Fatalf("asking for an unknown secret errored differently: %v", err)
	}
	if _, err := b.Approve(req.ID, "jam", 0); err == nil {
		t.Fatal("approving a request for a nonexistent secret succeeded")
	}
	// And the request is still pending rather than half-decided.
	pending, _ := b.Requests(StatePending, 10)
	if len(pending) != 1 {
		t.Errorf("a failed approval consumed the request: %v", pending)
	}
}

func TestAsksAreAudited(t *testing.T) {
	// An ask that was never approved is still worth being able to see.
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "reading issues", 600)
	if _, err := b.Deny(req.ID, "jam", "no"); err != nil {
		t.Fatal(err)
	}
	rows, err := b.Audit(50)
	if err != nil {
		t.Fatal(err)
	}
	var sawAsk, sawDenial bool
	for _, row := range rows {
		switch row["action"] {
		case "grant_requested":
			sawAsk = true
		case "grant_denied":
			sawDenial = true
		}
	}
	if !sawAsk || !sawDenial {
		t.Errorf("audit missing ask=%v denial=%v: %v", sawAsk, sawDenial, rows)
	}
}

func TestARunawayReasonIsTruncatedNotRejected(t *testing.T) {
	// A misbehaving agent must not be able to fill the table, and must also
	// not be blocked from asking at all — the ask is the safe path.
	_, b := readyVault(t)
	req, err := b.RequestGrant("github-token", "claude-4", "",
		strings.Repeat("please ", 5000), 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(req.Reason) > maxReasonLen {
		t.Errorf("reason kept at %d characters", len(req.Reason))
	}
}

func TestTTLIsCapped(t *testing.T) {
	_, b := readyVault(t)
	req, _ := b.RequestGrant("github-token", "claude-4", "", "x", 999999)
	if req.TTL > maxRequestTTL {
		t.Errorf("ttl = %d, want it capped at %d", req.TTL, maxRequestTTL)
	}
}

func TestPendingCountIsWhatABadgeShows(t *testing.T) {
	_, b := readyVault(t)
	if n, _ := b.PendingCount(); n != 0 {
		t.Fatalf("fresh instance has %d pending", n)
	}
	r1, _ := b.RequestGrant("github-token", "a1", "", "x", 600)
	b.RequestGrant("github-token", "a2", "", "y", 600)
	if n, _ := b.PendingCount(); n != 2 {
		t.Errorf("pending = %d, want 2", n)
	}
	if _, err := b.Approve(r1.ID, "jam", 0); err != nil {
		t.Fatal(err)
	}
	if n, _ := b.PendingCount(); n != 1 {
		t.Errorf("pending after one approval = %d, want 1", n)
	}
}
