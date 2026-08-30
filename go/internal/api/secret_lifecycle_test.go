package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/secrets"
)

// The operations an operator performs on a credential store, over HTTP.
//
// The property under all of them: none of these routes may return a value.
// Rotation history, expiry, use counts and leak findings are all things you
// need in order to decide what to do about a secret, and none of them require
// seeing it.

func vaultedServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	s, h := testServer(t)
	if err := s.Secrets.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	return s, h
}

func put(t *testing.T, h http.Handler, name, value, note string, meta map[string]any) {
	t.Helper()
	body := map[string]any{"name": name, "value": value, "note": note}
	if meta != nil {
		body["meta"] = meta
	}
	w := do(t, h, "POST", "/api/secrets", body)
	if w.Code != http.StatusCreated {
		t.Fatalf("put %s = %d: %s", name, w.Code, w.Body)
	}
}

func TestWritingASecretThroughTheAPIKeepsHistory(t *testing.T) {
	_, h := vaultedServer(t)
	put(t, h, "stripe", "sk_live_ONE", "", nil)
	put(t, h, "stripe", "sk_live_TWO", "quarterly rotation", nil)

	w := do(t, h, "GET", "/api/secrets/versions?name=stripe", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("versions = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Name     string            `json:"name"`
		Versions []secrets.Version `json:"versions"`
		Retained int               `json:"retained"`
	}
	decode(t, w, &out)
	if len(out.Versions) != 1 {
		t.Fatalf("%d versions, want 1", len(out.Versions))
	}
	if out.Versions[0].Note != "quarterly rotation" {
		t.Errorf("note = %q, want the reason the caller gave", out.Versions[0].Note)
	}
	if out.Versions[0].Value != "" {
		t.Error("the history endpoint returned a value")
	}
	if strings.Contains(w.Body.String(), "sk_live_ONE") {
		t.Fatal("an old secret value appeared in the versions response")
	}
}

func TestRollbackOverHTTP(t *testing.T) {
	s, h := vaultedServer(t)
	put(t, h, "k", "first", "", nil)
	put(t, h, "k", "second", "", nil)

	w := do(t, h, "POST", "/api/secrets/restore", map[string]any{"name": "k"})
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", w.Code, w.Body)
	}
	// Verified through the vault, not the response: the response must not be
	// able to tell us what the value is.
	got, err := s.Secrets.Get("k")
	if err != nil {
		t.Fatal(err)
	}
	if got != "first" {
		t.Errorf("after restore = %q, want first", got)
	}
	if strings.Contains(w.Body.String(), "first") {
		t.Error("the restore response contained the value")
	}
}

func TestRestoreDefaultsToTheMostRecentPreviousValue(t *testing.T) {
	s, h := vaultedServer(t)
	put(t, h, "k", "v1", "", nil)
	put(t, h, "k", "v2", "", nil)
	put(t, h, "k", "v3", "", nil)
	// No version given: a rollback almost always means "the one before this".
	w := do(t, h, "POST", "/api/secrets/restore", map[string]any{"name": "k"})
	if w.Code != http.StatusOK {
		t.Fatalf("restore = %d: %s", w.Code, w.Body)
	}
	if got, _ := s.Secrets.Get("k"); got != "v2" {
		t.Errorf("restored %q, want v2", got)
	}
}

func TestRestoreNeedsANameAndReportsABadVersion(t *testing.T) {
	_, h := vaultedServer(t)
	put(t, h, "k", "v1", "", nil)
	if w := do(t, h, "POST", "/api/secrets/restore", map[string]any{}); w.Code != http.StatusBadRequest {
		t.Errorf("restore with no name = %d, want 400", w.Code)
	}
	v := 9
	w := do(t, h, "POST", "/api/secrets/restore", map[string]any{"name": "k", "version": v})
	if w.Code != http.StatusBadRequest {
		t.Errorf("restore of a missing version = %d, want 400", w.Code)
	}
}

func TestDetailsDescribeWithoutRevealing(t *testing.T) {
	_, h := vaultedServer(t)
	put(t, h, "billing", "sk_live_SECRETVALUE", "", map[string]any{
		secrets.MetaNote: "stripe billing", secrets.MetaExpires: "2020-01-01"})
	put(t, h, "fresh", "another_SECRETVALUE", "", nil)

	w := do(t, h, "GET", "/api/secrets/details", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("details = %d: %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "SECRETVALUE") {
		t.Fatal("the details listing contained a secret value")
	}
	var out struct {
		Secrets        []secrets.Info `json:"secrets"`
		NeedsAttention int            `json:"needs_attention"`
	}
	decode(t, w, &out)
	if len(out.Secrets) != 2 {
		t.Fatalf("%d secrets described, want 2", len(out.Secrets))
	}
	byName := map[string]secrets.Info{}
	for _, i := range out.Secrets {
		byName[i.Name] = i
	}
	if byName["billing"].Status != secrets.StatusExpired {
		t.Errorf("status = %q, want expired", byName["billing"].Status)
	}
	if byName["billing"].Note != "stripe billing" {
		t.Errorf("note = %q", byName["billing"].Note)
	}
	if out.NeedsAttention != 1 {
		t.Errorf("needs_attention = %d, want 1 — counted server-side so every "+
			"surface agrees", out.NeedsAttention)
	}
}

func TestTheOldSecretsListStillReturnsBareNames(t *testing.T) {
	_, h := vaultedServer(t)
	put(t, h, "k", "v", "", map[string]any{secrets.MetaNote: "a note"})
	w := do(t, h, "GET", "/api/secrets", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("secrets = %d", w.Code)
	}
	// Widening the existing shape in place would have started shipping
	// timestamps and use counts to every caller that asked for names.
	if strings.Contains(w.Body.String(), "a note") {
		t.Error("the bare list grew fields; details is the route for that")
	}
}

func TestScanFindsCredentialsInNotesAndMasksThem(t *testing.T) {
	_, h := vaultedServer(t)
	w := do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Debugging",
		"body":  "# Debugging\n\nused AKIAIOSFODNN7EXAMPLE while testing\n",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("creating the note = %d: %s", w.Code, w.Body)
	}
	w = do(t, h, "GET", "/api/secrets/scan", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("scan = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Findings []secrets.Finding `json:"findings"`
		High     int               `json:"high"`
		Scanned  int               `json:"scanned"`
	}
	decode(t, w, &out)
	if out.High < 1 {
		t.Fatalf("no high-confidence finding: %+v", out)
	}
	if strings.Contains(w.Body.String(), "AKIAIOSFODNN7EXAMPLE") {
		t.Fatal("the scan response quoted the credential it found — that copies " +
			"the leak into a log and a ticket")
	}
	var found *secrets.Finding
	for i := range out.Findings {
		if out.Findings[i].Kind == "AWS access key" {
			found = &out.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("AWS key not among %+v", out.Findings)
	}
	if found.Path == "" || found.Line == 0 || found.Advice == "" {
		t.Errorf("finding is not actionable: %+v", *found)
	}
}

func TestACleanVaultScansClean(t *testing.T) {
	_, h := vaultedServer(t)
	do(t, h, "POST", "/api/notes", map[string]any{
		"title": "Ordinary", "body": "# Ordinary\n\nThe key is in the vault. password: changeme\n"})
	w := do(t, h, "GET", "/api/secrets/scan", nil)
	var out struct {
		Findings []secrets.Finding `json:"findings"`
	}
	decode(t, w, &out)
	if len(out.Findings) != 0 {
		t.Errorf("false positives on an ordinary note: %+v", out.Findings)
	}
}

// Every one of these routes is part of the administrative surface.
func TestTheNewCredentialRoutesAreGated(t *testing.T) {
	s, h := vaultedServer(t)
	s.AdminToken = "tok"
	gated := s.requireAdminToken(h)
	for _, path := range []string{
		"/api/secrets/details", "/api/secrets/versions?name=k", "/api/secrets/scan",
	} {
		w := doOn(t, gated, "GET", path)
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s without the admin token = %d, want 401", path, w.Code)
		}
	}
}

// doOn performs a bare GET against a wrapped handler.
func doOn(t *testing.T, h http.Handler, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

// ------------------------------------------------- namespaces and use limits

func TestDetailsCanBeScopedToANamespace(t *testing.T) {
	_, h := vaultedServer(t)
	for _, n := range []string{"prod/stripe", "prod/github", "dev/stripe", "production/other"} {
		put(t, h, n, "x", "", nil)
	}
	w := do(t, h, "GET", "/api/secrets/details?prefix=prod", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("details = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Secrets    []secrets.Info `json:"secrets"`
		Namespaces map[string]int `json:"namespaces"`
	}
	decode(t, w, &out)
	if len(out.Secrets) != 2 {
		t.Fatalf("%d secrets under prod, want 2: %+v", len(out.Secrets), out.Secrets)
	}
	for _, i := range out.Secrets {
		if strings.HasPrefix(i.Name, "production/") {
			t.Error(`"prod" selected "production/…" — a prefix compared as a ` +
				`string hands one namespace's secrets to a caller scoped to another`)
		}
	}
	// The namespace list is always the whole vault's, so a scoped view can
	// still offer the others.
	if out.Namespaces["dev"] != 1 || out.Namespaces["production"] != 1 {
		t.Errorf("namespaces = %v, want every namespace listed", out.Namespaces)
	}
}

func TestAGrantCanBeLimitedToACountOfUses(t *testing.T) {
	s, h := vaultedServer(t)
	put(t, h, "api", "the-value", "", nil)
	w := do(t, h, "POST", "/api/secrets/api/grant", map[string]any{
		"grantee": "agent", "scope": "https://example.com", "ttl_seconds": 300,
		"max_uses": 1})
	if w.Code != http.StatusOK {
		t.Fatalf("grant = %d: %s", w.Code, w.Body)
	}
	var out struct {
		Grant   string `json:"grant"`
		MaxUses int    `json:"max_uses"`
	}
	decode(t, w, &out)
	if out.MaxUses != 1 {
		t.Errorf("max_uses = %d, want 1 echoed back", out.MaxUses)
	}
	grants, err := s.Broker.List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, g := range grants {
		if g.Token == out.Grant {
			found = true
			if g.MaxUses != 1 {
				t.Errorf("stored max_uses = %d, want 1", g.MaxUses)
			}
		}
	}
	if !found {
		t.Error("the grant was not listed")
	}
}

// An agent that knows it needs one call should be able to ask for one call;
// the approver is otherwise guessing.
func TestAnAgentCanBoundItsOwnRequest(t *testing.T) {
	s, h := vaultedServer(t)
	put(t, h, "api", "the-value", "", nil)
	w := do(t, h, "POST", "/api/secrets/requests", map[string]any{
		"secret": "api", "grantee": "agent", "scope": "https://example.com",
		"reason": "post one webhook", "ttl_seconds": 300, "max_uses": 1})
	if w.Code != http.StatusAccepted {
		t.Fatalf("request = %d: %s", w.Code, w.Body)
	}
	var req struct {
		ID      string `json:"id"`
		MaxUses int    `json:"max_uses"`
	}
	decode(t, w, &req)
	if req.MaxUses != 1 {
		t.Errorf("max_uses = %d on the ask, want 1", req.MaxUses)
	}
	// And approving must carry the bound through to the grant rather than
	// quietly widening it.
	if _, err := s.Broker.Approve(req.ID, "operator", 0); err != nil {
		t.Fatal(err)
	}
	grants, _ := s.Broker.List()
	if len(grants) == 0 {
		t.Fatal("approval minted no grant")
	}
	if grants[0].MaxUses != 1 {
		t.Errorf("granted max_uses = %d, want the 1 that was asked for — an "+
			"approval that widens the ask is not the ask", grants[0].MaxUses)
	}
}
