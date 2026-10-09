package secrets

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/secrets/providers"
)

// installFakeOp puts a counting fake `op` on PATH. The counter lives in a file
// because the child process gets a scrubbed environment.
func installFakeOp(t *testing.T, token string) (counter string) {
	t.Helper()
	dir := t.TempDir()
	counter = filepath.Join(dir, "calls")
	script := "#!/bin/sh\n" +
		"[ \"$OP_SERVICE_ACCOUNT_TOKEN\" = \"" + token + "\" ] || { echo '[ERROR] bad token' >&2; exit 1; }\n" +
		"echo x >> " + counter + "\n" +
		"printf 'ext-secret-value-42'\n"
	if err := os.WriteFile(filepath.Join(dir, "op"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return counter
}

func calls(t *testing.T, f string) int {
	raw, _ := os.ReadFile(f)
	return strings.Count(string(raw), "x")
}

func opCfg(token string, extra map[string]string) providers.Config {
	s := map[string]string{}
	for k, v := range extra {
		s[k] = v
	}
	return providers.Config{Name: "op", Kind: "onepassword", Settings: s,
		Secrets: map[string]string{"service_account_token": token}}
}

func TestExternalHandleEndToEnd(t *testing.T) {
	v, b := testVault(t)
	if err := v.Initialize("correct horse battery"); err != nil {
		t.Fatal(err)
	}
	installFakeOp(t, "ops_tok_12345")
	if err := v.AddProvider(opCfg("ops_tok_12345", nil)); err != nil {
		t.Fatal(err)
	}
	name, err := v.ResolveProviderName("op://agents/GH/token", "")
	if err != nil || name != "op" {
		t.Fatalf("%q %v", name, err)
	}
	if err := v.Link("gh", name, "op://agents/GH/token", "github"); err != nil {
		t.Fatal(err)
	}

	// the target echoes the credential back, which must not reach the caller
	var seen string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		_, _ = w.Write([]byte("you sent " + seen))
	}))
	defer srv.Close()
	tok, err := b.Grant(GrantSpec{Secret: "gh", Grantee: "agent", Scope: srv.URL, TTLSeconds: 60})
	if err != nil {
		t.Fatal(err)
	}
	res, err := b.Use(tok, "GET", srv.URL+"/x", "Authorization", "")
	if err != nil {
		t.Fatal(err)
	}
	if seen != "ext-secret-value-42" {
		t.Fatalf("server saw %q", seen)
	}
	if body := res["body"].(string); strings.Contains(body, "ext-secret-value-42") {
		t.Fatalf("response leaked the credential: %q", body)
	}

	// inventory shows provider and ref, never a value
	grants, _ := b.List()
	if len(grants) != 1 || grants[0].Provider != "op" || grants[0].Ref != "op://agents/GH/token" {
		t.Fatalf("%+v", grants)
	}
	info, _ := v.Describe()
	if len(info) != 1 || info[0].Provider != "op" {
		t.Fatalf("%+v", info)
	}
	// audit names the provider, and nothing in it holds the value
	entries, _ := b.Audit(50)
	found := false
	for _, e := range entries {
		for _, f := range e {
			if strings.Contains(f.(string), "ext-secret-value-42") || strings.Contains(f.(string), "ops_tok_12345") {
				t.Fatalf("audit leaks: %v", e)
			}
		}
		if e["action"] == "broker" && strings.Contains(e["detail"].(string), "provider=op") {
			found = true
		}
	}
	if !found {
		t.Fatalf("broker audit row lacks provider: %v", entries)
	}
}

func TestExternalNothingOnDiskInClear(t *testing.T) {
	v, _ := testVault(t)
	_ = v.Initialize("correct horse battery")
	installFakeOp(t, "ops_tok_unique_777")
	_ = v.AddProvider(opCfg("ops_tok_unique_777", nil))
	_ = v.Link("gh", "op", "op://agents/GH/token", "")
	if _, err := v.Get("gh"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(v.Path)
	for _, s := range []string{"ops_tok_unique_777", "ext-secret-value-42"} {
		if strings.Contains(string(raw), s) {
			t.Fatalf("%q is in the vault file in the clear", s)
		}
	}
}

func TestExternalLockedProviderIsClearErrorAndKeepsSingleUseGrant(t *testing.T) {
	v, b := testVault(t)
	_ = v.Initialize("correct horse battery")
	installFakeOp(t, "right-token-1")
	_ = v.AddProvider(opCfg("revoked-token-9", nil)) // wrong token: manager "locked"
	_ = v.Link("gh", "op", "op://a/b/c", "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	tok, err := b.Grant(GrantSpec{Secret: "gh", Grantee: "agent", Scope: srv.URL, MaxUses: 1})
	if err != nil {
		t.Fatalf("grant must not need the manager unlocked: %v", err)
	}
	_, err = b.Use(tok, "GET", srv.URL, "", "")
	if !errors.Is(err, providers.ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "revoked-token-9") {
		t.Fatal("error leaks unlock material")
	}
	// fix the provider; the one-shot grant is still usable
	_ = v.AddProvider(opCfg("right-token-1", nil))
	if _, err := b.Use(tok, "GET", srv.URL, "", ""); err != nil {
		t.Fatalf("grant was burned by the failed attempt: %v", err)
	}
}

func TestExternalCache(t *testing.T) {
	v, _ := testVault(t)
	_ = v.Initialize("correct horse battery")
	counter := installFakeOp(t, "tok-abcd")
	_ = v.AddProvider(opCfg("tok-abcd", nil))
	_ = v.Link("gh", "op", "op://a/b/c", "")
	for i := 0; i < 3; i++ {
		_, _ = v.Get("gh")
	}
	if n := calls(t, counter); n != 3 {
		t.Fatalf("default cache must be off, provider called %d times", n)
	}

	cached := opCfg("tok-abcd", map[string]string{"cache_seconds": "30"})
	_ = v.AddProvider(cached)
	now := time.Now()
	old := Now
	Now = func() time.Time { return now }
	defer func() { Now = old }()
	for i := 0; i < 3; i++ {
		_, _ = v.Get("gh")
	}
	if n := calls(t, counter); n != 4 {
		t.Fatalf("cached: want 1 more call, total %d", n)
	}
	now = now.Add(31 * time.Second)
	_, _ = v.Get("gh")
	if n := calls(t, counter); n != 5 {
		t.Fatalf("expired entry should refetch, total %d", n)
	}
	v.Lock()
	if len(v.ext.cache) != 0 {
		t.Fatal("lock must drop the cache")
	}
}

func TestExternalGuardrails(t *testing.T) {
	v, _ := testVault(t)
	_ = v.Initialize("correct horse battery")
	installFakeOp(t, "tok-abcd")
	_ = v.AddProvider(opCfg("tok-abcd", nil))
	_ = v.Put("plain", "v", nil)
	if err := v.Link("plain", "op", "op://a/b/c", ""); err == nil {
		t.Fatal("link must not overwrite a stored value")
	}
	_ = v.Link("gh", "op", "op://a/b/c", "")
	if err := v.PutVersioned("gh", "x", nil, ""); err == nil {
		t.Fatal("writing a value into a linked handle must be refused")
	}
	if err := v.RemoveProvider("op"); err == nil {
		t.Fatal("provider with links must not be removable")
	}
	if err := v.Link("bad", "op", "op://only/two", ""); err == nil {
		t.Fatal("malformed ref accepted")
	}
	if err := v.Link("bad", "nope", "op://a/b/c", ""); err == nil {
		t.Fatal("unknown provider accepted")
	}
	_ = v.Unlink("gh")
	if err := v.RemoveProvider("op"); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Get("gh"); err == nil {
		t.Fatal("unlinked handle should be gone")
	}
}

func TestExternalSurvivesRestartAndPassphraseChange(t *testing.T) {
	v, _ := testVault(t)
	_ = v.Initialize("correct horse battery")
	installFakeOp(t, "tok-abcd")
	_ = v.AddProvider(opCfg("tok-abcd", nil))
	_ = v.Link("gh", "op", "op://a/b/c", "")

	if err := v.ChangePassphrase("correct horse battery", "another long passphrase", nil); err != nil {
		t.Fatal(err)
	}
	v2 := New(filepath.Dir(v.Path))
	if err := v2.Unlock("another long passphrase"); err != nil {
		t.Fatal(err)
	}
	ps, err := v2.Providers()
	if err != nil || len(ps) != 1 || ps[0].Links != 1 || ps[0].SecretKeys[0] != "service_account_token" {
		t.Fatalf("%+v %v", ps, err)
	}
	if got, err := v2.Get("gh"); err != nil || got != "ext-secret-value-42" {
		t.Fatalf("%q %v", got, err)
	}
	if msg, err := v2.TestProvider("op"); err != nil || msg == "" {
		t.Fatalf("%q %v", msg, err)
	}
	v2.Lock()
	if _, err := v2.Get("gh"); !errors.Is(err, ErrLocked) {
		t.Fatalf("locked vault must not resolve: %v", err)
	}
}

func TestProviderNameSelection(t *testing.T) {
	v, _ := testVault(t)
	_ = v.Initialize("correct horse battery")
	if _, err := v.ResolveProviderName("op://a/b/c", ""); err == nil {
		t.Fatal("no provider configured should error")
	}
	_ = v.AddProvider(opCfg("tok-abcd", nil))
	c2 := opCfg("tok-efgh", nil)
	c2.Name = "op2"
	_ = v.AddProvider(c2)
	if _, err := v.ResolveProviderName("op://a/b/c", ""); err == nil {
		t.Fatal("ambiguity should require --provider")
	}
	if n, err := v.ResolveProviderName("op://a/b/c", "op2"); err != nil || n != "op2" {
		t.Fatalf("%q %v", n, err)
	}
	if _, err := v.ResolveProviderName("pass://x", "op2"); err == nil {
		t.Fatal("kind mismatch accepted")
	}
}
