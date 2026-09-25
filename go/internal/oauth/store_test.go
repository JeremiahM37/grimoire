package oauth

import (
	"path/filepath"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "oauth.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestCreateClientPublicVsConfidential(t *testing.T) {
	s := openTestStore(t)

	pub, secret, err := s.CreateClient("Public Client", []string{"https://claude.ai/callback"}, "")
	if err != nil {
		t.Fatalf("CreateClient (public): %v", err)
	}
	if secret != "" {
		t.Fatal("a public client (token_endpoint_auth_method none) must not receive a secret")
	}
	if pub.hasSecret() {
		t.Fatal("public client must not report hasSecret")
	}

	conf, secret2, err := s.CreateClient("Confidential Client", []string{"https://example.com/callback"}, "client_secret_post")
	if err != nil {
		t.Fatalf("CreateClient (confidential): %v", err)
	}
	if secret2 == "" {
		t.Fatal("a confidential client must receive a secret on creation")
	}
	if !conf.hasSecret() {
		t.Fatal("confidential client must report hasSecret")
	}
	if !s.CheckClientSecret(conf.ID, secret2) {
		t.Fatal("the returned secret must verify against the stored hash")
	}
	if s.CheckClientSecret(conf.ID, "wrong-secret") {
		t.Fatal("a wrong secret must not verify")
	}
	if s.CheckClientSecret(pub.ID, "anything") {
		t.Fatal("a public client has no secret to verify")
	}
}

func TestAuthCodeSingleUse(t *testing.T) {
	s := openTestStore(t)
	client, _, _ := s.CreateClient("C", []string{"https://x.example/cb"}, "")

	ac := AuthCode{
		ClientID: client.ID, RedirectURI: "https://x.example/cb",
		Scopes: []string{ScopeNotesRead}, Resource: "https://mcp.example/mcp",
		CodeChallenge: "chal", CodeChallengeMethod: "S256",
	}
	if err := s.SaveAuthCode("code123", ac, 2*time.Minute); err != nil {
		t.Fatalf("SaveAuthCode: %v", err)
	}
	got, err := s.ConsumeAuthCode("code123")
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	if got.ClientID != client.ID || got.RedirectURI != ac.RedirectURI {
		t.Fatalf("unexpected auth code contents: %+v", got)
	}
	if _, err := s.ConsumeAuthCode("code123"); err != ErrNotFound {
		t.Fatalf("second consume: want ErrNotFound, got %v", err)
	}
}

func TestAuthCodeExpiry(t *testing.T) {
	s := openTestStore(t)
	client, _, _ := s.CreateClient("C", []string{"https://x.example/cb"}, "")
	orig := Now
	defer func() { Now = orig }()

	Now = func() time.Time { return time.Unix(1000, 0) }
	ac := AuthCode{ClientID: client.ID, RedirectURI: "https://x.example/cb", CodeChallenge: "c", CodeChallengeMethod: "S256"}
	if err := s.SaveAuthCode("expiring", ac, time.Second); err != nil {
		t.Fatalf("SaveAuthCode: %v", err)
	}
	Now = func() time.Time { return time.Unix(2000, 0) } // well past the 1s TTL
	if _, err := s.ConsumeAuthCode("expiring"); err != ErrExpired {
		t.Fatalf("want ErrExpired, got %v", err)
	}
	// Consumed either way — a second attempt is "not found", not "expired
	// again", because the row is gone.
	if _, err := s.ConsumeAuthCode("expiring"); err != ErrNotFound {
		t.Fatalf("want ErrNotFound on replay of an expired code, got %v", err)
	}
}

func TestIssueLookupAndRevokeToken(t *testing.T) {
	s := openTestStore(t)
	client, _, _ := s.CreateClient("C", []string{"https://x.example/cb"}, "")

	pair, err := s.IssueTokenPair(client.ID, []string{ScopeNotesRead, ScopeMemory}, "https://mcp.example/mcp")
	if err != nil {
		t.Fatalf("IssueTokenPair: %v", err)
	}
	if pair.AccessToken == "" || pair.RefreshToken == "" || pair.AccessToken == pair.RefreshToken {
		t.Fatalf("expected two distinct opaque tokens, got %+v", pair)
	}

	rec, err := s.LookupAccessToken(pair.AccessToken)
	if err != nil {
		t.Fatalf("LookupAccessToken: %v", err)
	}
	if rec.ClientID != client.ID || rec.Resource != "https://mcp.example/mcp" {
		t.Fatalf("unexpected token record: %+v", rec)
	}
	if !scopeGranted(rec.Scopes, ScopeNotesRead) || !scopeGranted(rec.Scopes, ScopeMemory) {
		t.Fatalf("expected both granted scopes, got %v", rec.Scopes)
	}

	if err := s.RevokeToken(pair.AccessToken); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	if _, err := s.LookupAccessToken(pair.AccessToken); err != ErrNotFound {
		t.Fatalf("want ErrNotFound after revoke, got %v", err)
	}
	// Revoking an already-gone token is not an error (RFC 7009 semantics),
	// exercised again at the HTTP layer in server_test.go.
	if err := s.RevokeToken(pair.AccessToken); err != nil {
		t.Fatalf("revoking twice should be harmless, got %v", err)
	}
}

func TestRotateRefreshInvalidatesOldToken(t *testing.T) {
	s := openTestStore(t)
	client, _, _ := s.CreateClient("C", []string{"https://x.example/cb"}, "")
	pair, err := s.IssueTokenPair(client.ID, []string{ScopeNotesRead}, "https://mcp.example/mcp")
	if err != nil {
		t.Fatalf("IssueTokenPair: %v", err)
	}

	next, err := s.RotateRefresh(pair.RefreshToken, client.ID)
	if err != nil {
		t.Fatalf("RotateRefresh: %v", err)
	}
	if next.RefreshToken == pair.RefreshToken || next.AccessToken == pair.AccessToken {
		t.Fatal("rotation must issue fresh tokens, not reuse the old ones")
	}
	if _, err := s.RotateRefresh(pair.RefreshToken, client.ID); err != ErrNotFound {
		t.Fatalf("the old refresh token must be dead after rotation, got %v", err)
	}
	if _, err := s.LookupAccessToken(pair.AccessToken); err != nil {
		// The old ACCESS token is intentionally left alone by rotation — it
		// simply expires on its own short TTL. Document that here so a
		// future change that starts revoking it is a deliberate one.
		t.Fatalf("rotation should not itself revoke the prior access token: %v", err)
	}

	other, _, _ := s.CreateClient("Other", []string{"https://y.example/cb"}, "")
	if _, err := s.RotateRefresh(next.RefreshToken, other.ID); err != ErrNotFound {
		t.Fatalf("a refresh token presented by the wrong client must be refused, got %v", err)
	}
}

func TestRevokeClientRemovesAllItsTokens(t *testing.T) {
	s := openTestStore(t)
	a, _, _ := s.CreateClient("A", []string{"https://a.example/cb"}, "")
	b, _, _ := s.CreateClient("B", []string{"https://b.example/cb"}, "")
	pairA, _ := s.IssueTokenPair(a.ID, []string{ScopeNotesRead}, "r")
	pairB, _ := s.IssueTokenPair(b.ID, []string{ScopeNotesRead}, "r")

	if err := s.RevokeClient(a.ID); err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}
	if _, err := s.LookupAccessToken(pairA.AccessToken); err != ErrNotFound {
		t.Fatal("client A's token should be gone")
	}
	if _, err := s.LookupAccessToken(pairB.AccessToken); err != nil {
		t.Fatal("client B's token must be unaffected by revoking client A")
	}
	if _, err := s.GetClient(a.ID); err != nil {
		t.Fatal("revoking a client's tokens must not delete the client registration itself")
	}
}

func TestListClients(t *testing.T) {
	s := openTestStore(t)
	c, _, _ := s.CreateClient("Listed Client", []string{"https://x.example/cb"}, "")
	if _, err := s.IssueTokenPair(c.ID, []string{ScopeNotesRead, ScopeCredentials}, "r"); err != nil {
		t.Fatalf("IssueTokenPair: %v", err)
	}
	list, err := s.ListClients()
	if err != nil {
		t.Fatalf("ListClients: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("expected 1 client, got %d", len(list))
	}
	if list[0].ID != c.ID || list[0].ActiveTokens != 1 {
		t.Fatalf("unexpected summary: %+v", list[0])
	}
	if !scopeGranted(list[0].Scopes, ScopeCredentials) {
		t.Fatalf("expected the credentials scope to show up in the summary, got %v", list[0].Scopes)
	}
}
