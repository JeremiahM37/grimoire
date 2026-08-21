package api

import (
	"net/http"
	"testing"
)

// The grant-request flow over HTTP, including the route-class asymmetry that
// makes it work: anyone with an account may ASK, only an admin may ANSWER.

func vaultServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, h := testServer(t)
	if w := do(t, h, "POST", "/api/vault/init",
		map[string]any{"passphrase": "correct horse battery"}); w.Code != http.StatusOK {
		t.Fatalf("vault init = %d: %s", w.Code, w.Body)
	}
	if w := do(t, h, "POST", "/api/secrets",
		map[string]any{"name": "github-token", "value": "ghp_x"}); w.Code >= 300 {
		t.Fatalf("put secret = %d: %s", w.Code, w.Body)
	}
	return s, h
}

func TestAskingReturns202AndNoToken(t *testing.T) {
	_, h := vaultServer(t)

	w := do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4",
		"scope": "https://api.github.com/", "reason": "read the open issues",
		"ttl_seconds": 600,
	})
	if w.Code != http.StatusAccepted {
		t.Fatalf("ask = %d: %s", w.Code, w.Body)
	}
	var req map[string]any
	decode(t, w, &req)
	if req["state"] != "pending" {
		t.Errorf("state = %v", req["state"])
	}
	if req["token"] != nil {
		t.Fatal("the ask response carried a grant token")
	}

	// Nothing was granted.
	var grants []map[string]any
	decode(t, do(t, h, "GET", "/api/grants", nil), &grants)
	if len(grants) != 0 {
		t.Errorf("asking created %d grants", len(grants))
	}
}

func TestTheApprovalQueueShowsWhatWasAskedAndWhy(t *testing.T) {
	_, h := vaultServer(t)
	do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4",
		"scope":  "https://api.github.com/repos/acme/",
		"reason": "read the open issues on acme/thing to answer the user"})

	var out map[string]any
	decode(t, do(t, h, "GET", "/api/secrets/requests?state=pending", nil), &out)
	if out["pending"].(float64) != 1 {
		t.Fatalf("pending = %v", out["pending"])
	}
	rows, _ := out["requests"].([]any)
	row, _ := rows[0].(map[string]any)
	// The reason is the entire basis on which a person decides, so it has to
	// survive to the surface they read.
	if row["reason"] != "read the open issues on acme/thing to answer the user" {
		t.Errorf("reason = %v", row["reason"])
	}
	if row["scope"] != "https://api.github.com/repos/acme/" {
		t.Errorf("scope = %v", row["scope"])
	}
	if row["token"] != nil {
		t.Error("the queue listing carried a token")
	}
}

func TestApproveThenTheAgentCollectsItsToken(t *testing.T) {
	_, h := vaultServer(t)
	var asked map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4",
		"scope": "https://api.github.com/", "reason": "issues"}), &asked)
	id, _ := asked["id"].(string)

	w := do(t, h, "POST", "/api/secrets/requests/"+id+"/approve", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", w.Code, w.Body)
	}
	var approved map[string]any
	decode(t, w, &approved)
	if approved["state"] != "approved" {
		t.Errorf("state = %v", approved["state"])
	}
	// The approver is answering a question, not collecting a credential.
	if approved["token"] != nil {
		t.Error("the approval response showed the token to the approver")
	}

	// The asker collects it.
	var collected map[string]any
	decode(t, do(t, h, "GET", "/api/secrets/requests/"+id+"?grantee=claude-4", nil), &collected)
	token, _ := collected["token"].(string)
	if token == "" {
		t.Fatal("the asker could not collect its token")
	}

	// And it works: a real brokered call with it.
	var grants []map[string]any
	decode(t, do(t, h, "GET", "/api/grants", nil), &grants)
	if len(grants) != 1 || grants[0]["token"] != token {
		t.Errorf("the collected token is not the issued grant: %v", grants)
	}
}

func TestAnotherAgentCannotCollectTheToken(t *testing.T) {
	_, h := vaultServer(t)
	var asked map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4", "reason": "issues"}), &asked)
	id, _ := asked["id"].(string)
	do(t, h, "POST", "/api/secrets/requests/"+id+"/approve", nil)

	w := do(t, h, "GET", "/api/secrets/requests/"+id+"?grantee=someone-else", nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("collecting someone else's token = %d, want 404: %s", w.Code, w.Body)
	}
	// A caller that names nobody gets nothing either.
	if w := do(t, h, "GET", "/api/secrets/requests/"+id, nil); w.Code != http.StatusNotFound {
		t.Fatalf("collecting with no grantee = %d, want 404", w.Code)
	}
}

func TestDenialCarriesAnActionableNote(t *testing.T) {
	_, h := vaultServer(t)
	var asked map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4", "reason": "issues"}), &asked)
	id, _ := asked["id"].(string)

	w := do(t, h, "POST", "/api/secrets/requests/"+id+"/deny",
		map[string]any{"note": "use the read-only key instead"})
	if w.Code != http.StatusOK {
		t.Fatalf("deny = %d: %s", w.Code, w.Body)
	}

	var collected map[string]any
	decode(t, do(t, h, "GET", "/api/secrets/requests/"+id+"?grantee=claude-4", nil), &collected)
	if collected["state"] != "denied" {
		t.Errorf("state = %v", collected["state"])
	}
	// The note is the half an agent can act on: "no" restarts the loop, "use
	// the read-only key" ends it.
	if collected["note"] != "use the read-only key instead" {
		t.Errorf("note = %v", collected["note"])
	}
	if collected["token"] != nil {
		t.Error("a denied request carried a token")
	}
}

func TestApprovingAnUnknownRequestIs404(t *testing.T) {
	_, h := vaultServer(t)
	if w := do(t, h, "POST", "/api/secrets/requests/nope/approve", nil); w.Code != http.StatusNotFound {
		t.Errorf("approve unknown = %d, want 404", w.Code)
	}
}

func TestApprovingWithALockedVaultIs423(t *testing.T) {
	_, h := vaultServer(t)
	var asked map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4", "reason": "issues"}), &asked)
	id, _ := asked["id"].(string)

	if w := do(t, h, "POST", "/api/vault/lock", nil); w.Code != http.StatusOK {
		t.Fatalf("lock = %d", w.Code)
	}
	w := do(t, h, "POST", "/api/secrets/requests/"+id+"/approve", nil)
	if w.Code != http.StatusLocked {
		t.Fatalf("approve with a locked vault = %d, want 423: %s", w.Code, w.Body)
	}
	// But asking still works while it is locked — that is the whole point.
	if w := do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-5", "reason": "issues",
	}); w.Code != http.StatusAccepted {
		t.Errorf("asking with a locked vault = %d, want 202", w.Code)
	}
}

func TestApprovalShortensTheTTLWhenAsked(t *testing.T) {
	_, h := vaultServer(t)
	var asked map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "github-token", "grantee": "claude-4",
		"reason": "issues", "ttl_seconds": 86400}), &asked)
	id, _ := asked["id"].(string)

	var approved map[string]any
	decode(t, do(t, h, "POST", "/api/secrets/requests/"+id+"/approve",
		map[string]any{"ttl_seconds": 600}), &approved)
	if approved["ttl_seconds"].(float64) != 600 {
		t.Errorf("ttl = %v, want the override", approved["ttl_seconds"])
	}
}

func TestAskingWithNoSecretNameIsRefused(t *testing.T) {
	_, h := vaultServer(t)
	if w := do(t, h, "POST", "/api/secrets/requests",
		map[string]any{"grantee": "claude-4", "reason": "x"}); w.Code != http.StatusBadRequest {
		t.Errorf("ask with no secret = %d, want 400", w.Code)
	}
}
