package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeBin installs an executable shell script named name on PATH. Tests never
// touch a real password manager: these stand in for bw, bws, op, etc.
func fakeBin(t *testing.T, name, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func mk(t *testing.T, cfg Config) Provider {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "t"
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func ctx() context.Context { return context.Background() }

const bwScript = `
echo "$@" >> "$BW_ARGLOG"
case "$1" in
status) if [ -f "$BITWARDENCLI_APPDATA_DIR/loggedin" ]; then echo '{"status":"locked"}'; else echo '{"status":"unauthenticated"}'; fi ;;
config) exit 0 ;;
login) if [ "$BW_CLIENTID" = "cid" ] && [ "$BW_CLIENTSECRET" = "csecret" ]; then touch "$BITWARDENCLI_APPDATA_DIR/loggedin"; exit 0; fi; echo "Invalid API key" >&2; exit 1 ;;
unlock) if [ "$BW_PASSWORD" = "master-pass-xyz" ]; then echo SESSION-OK; exit 0; fi; echo "Invalid master password." >&2; exit 1 ;;
get)
  if [ "$BW_SESSION" != "SESSION-OK" ]; then echo "Vault is locked." >&2; exit 1; fi
  if [ "$2" = "item" ]; then echo '{"fields":[{"name":"api token","value":"custom-val"}]}'; exit 0; fi
  [ "$2" = "password" ] && printf 'pw-for-%s\n' "$3" && exit 0
  [ "$2" = "username" ] && printf 'user-for-%s\n' "$3" && exit 0
  exit 1 ;;
list)
  if [ "$BW_SESSION" != "SESSION-OK" ]; then echo "Vault is locked." >&2; exit 1; fi
  if [ "$2" = "folders" ]; then echo '[{"id":"f1","name":"agents"}]'; exit 0; fi
  echo '[{"id":"i1","name":"GitHub token","folderId":"f1"},{"id":"i2","name":"Stripe","folderId":"f1"}]' ;;
esac`

func TestBitwardenCLIWithSession(t *testing.T) {
	fakeBin(t, "bw", bwScript)
	p := mk(t, Config{Kind: "bitwarden", Secrets: map[string]string{"session": "SESSION-OK"}})
	got, err := p.Resolve(ctx(), "bitwarden://abc-123/password")
	if err != nil || got != "pw-for-abc-123" {
		t.Fatalf("got %q, %v", got, err)
	}
	if got, _ := p.Resolve(ctx(), "bitwarden://abc-123/username"); got != "user-for-abc-123" {
		t.Fatalf("username: %q", got)
	}
	if got, err := p.Resolve(ctx(), "bitwarden://abc-123/field/API Token"); err != nil || got != "custom-val" {
		t.Fatalf("custom field: %q %v", got, err)
	}
	if _, err := p.Resolve(ctx(), "bitwarden://abc-123/field/missing"); err == nil {
		t.Fatal("missing custom field should error")
	}
}

func TestBitwardenStaleSessionIsUnavailableNoFallback(t *testing.T) {
	fakeBin(t, "bw", bwScript)
	p := mk(t, Config{Kind: "bitwarden", Secrets: map[string]string{"session": "EXPIRED"}})
	_, err := p.Resolve(ctx(), "bitwarden://abc/password")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable, got %v", err)
	}
}

func TestBitwardenMasterPasswordLoginUnlockAndNoArgvLeak(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv.log")
	// the child sees a scrubbed environment, so the log path is baked in
	fakeBin(t, "bw", strings.ReplaceAll(bwScript, `"$BW_ARGLOG"`, log))
	app := t.TempDir()
	p := mk(t, Config{Kind: "bitwarden",
		Settings: map[string]string{"appdata_dir": app, "server": "https://vw.example"},
		Secrets: map[string]string{"master_password": "master-pass-xyz",
			"client_id": "cid", "client_secret": "csecret"}})
	got, err := p.Resolve(ctx(), "bitwarden://item9")
	if err != nil || got != "pw-for-item9" {
		t.Fatalf("got %q, %v", got, err)
	}
	argv, _ := os.ReadFile(log)
	for _, secret := range []string{"master-pass-xyz", "csecret", "SESSION-OK"} {
		if strings.Contains(string(argv), secret) {
			t.Fatalf("%s leaked into argv:\n%s", secret, argv)
		}
	}
	if !strings.Contains(string(argv), "config server https://vw.example") {
		t.Fatalf("custom server not configured:\n%s", argv)
	}
	// Close drops the session; the next call unlocks again.
	p.Close()
	if got, err := p.Resolve(ctx(), "bitwarden://item9"); err != nil || got == "" {
		t.Fatalf("after close: %q %v", got, err)
	}
}

func TestBitwardenWrongMasterPasswordIsUnavailable(t *testing.T) {
	fakeBin(t, "bw", bwScript)
	p := mk(t, Config{Kind: "bitwarden", Settings: map[string]string{"appdata_dir": t.TempDir()},
		Secrets: map[string]string{"master_password": "wrong-pw-1234", "client_id": "cid", "client_secret": "csecret"}})
	_, err := p.Resolve(ctx(), "bitwarden://x/password")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong-pw-1234") {
		t.Fatal("error leaks the password")
	}
}

func TestBitwardenListItems(t *testing.T) {
	fakeBin(t, "bw", bwScript)
	p := mk(t, Config{Kind: "bitwarden", Secrets: map[string]string{"session": "SESSION-OK"}})
	items, err := p.(Lister).ListItems(ctx(), "agents")
	if err != nil || len(items) != 2 || items[0].Ref != "bitwarden://i1/password" {
		t.Fatalf("%v %v", items, err)
	}
	if _, err := p.(Lister).ListItems(ctx(), "nope"); err == nil {
		t.Fatal("unknown folder should error")
	}
}

func TestBitwardenServe(t *testing.T) {
	locked := true
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, data any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "data": data})
	}
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		st := "unlocked"
		if locked {
			st = "locked"
		}
		reply(w, map[string]any{"template": map[string]any{"status": st}})
	})
	mux.HandleFunc("/unlock", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Password string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Password != "master-pass-xyz" {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "message": "Invalid master password."})
			return
		}
		locked = false
		reply(w, map[string]any{"raw": "s"})
	})
	mux.HandleFunc("/object/password/", func(w http.ResponseWriter, r *http.Request) {
		reply(w, map[string]any{"object": "string", "data": "serve-pw-" + strings.TrimPrefix(r.URL.Path, "/object/password/")})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	p := mk(t, Config{Kind: "bitwarden", Settings: map[string]string{"serve_url": srv.URL},
		Secrets: map[string]string{"master_password": "master-pass-xyz"}})
	got, err := p.Resolve(ctx(), "bitwarden://zz/password")
	if err != nil || got != "serve-pw-zz" {
		t.Fatalf("got %q %v", got, err)
	}
	// locked and no master password -> unavailable
	locked = true
	p2 := mk(t, Config{Kind: "bitwarden", Settings: map[string]string{"serve_url": srv.URL}, Secrets: map[string]string{"session": "unused"}})
	if _, err := p2.Resolve(ctx(), "bitwarden://zz/password"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestBitwardenServeMustBeLoopback(t *testing.T) {
	_, err := New(Config{Name: "x", Kind: "bitwarden", Settings: map[string]string{"serve_url": "http://10.0.0.5:8087"}})
	if err == nil {
		t.Fatal("remote bw serve must be refused")
	}
}

func TestBitwardenSecretsManagerCLI(t *testing.T) {
	fakeBin(t, "bws", `
[ "$BWS_ACCESS_TOKEN" = "0.tok.sec:key" ] || { echo "Missing access token" >&2; exit 1; }
case "$1 $2" in
"secret get") echo "{\"id\":\"$3\",\"key\":\"K\",\"value\":\"bws-val-$3\",\"note\":\"n1\"}" ;;
"project list") echo '[]' ;;
esac`)
	p := mk(t, Config{Kind: "bitwarden-sm", Secrets: map[string]string{"access_token": "0.tok.sec:key"}})
	if got, err := p.Resolve(ctx(), "bws://sid-1"); err != nil || got != "bws-val-sid-1" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := p.Resolve(ctx(), "bws://sid-1/note"); got != "n1" {
		t.Fatalf("note %q", got)
	}
	bad := mk(t, Config{Kind: "bitwarden-sm", Secrets: map[string]string{"access_token": "nope-token"}})
	if _, err := bad.Resolve(ctx(), "bws://sid-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestOnePasswordCLI(t *testing.T) {
	fakeBin(t, "op", `
[ "$OP_SERVICE_ACCOUNT_TOKEN" = "ops_good" ] || { echo "[ERROR] invalid token" >&2; exit 1; }
if [ "$1" = "read" ]; then printf 'val-of-%s' "$3"; exit 0; fi
echo '[]'`)
	p := mk(t, Config{Kind: "onepassword", Secrets: map[string]string{"service_account_token": "ops_good"}})
	if got, err := p.Resolve(ctx(), "op://agents/GitHub/token"); err != nil || got != "val-of-op://agents/GitHub/token" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := p.Resolve(ctx(), "op://onlytwo/parts"); err == nil {
		t.Fatal("malformed ref")
	}
	bad := mk(t, Config{Kind: "onepassword", Secrets: map[string]string{"service_account_token": "ops_bad"}})
	if _, err := bad.Resolve(ctx(), "op://a/b/c"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestOnePasswordConnect(t *testing.T) {
	mux := http.NewServeMux()
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer connect-tok" {
				w.WriteHeader(401)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/v1/vaults", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "v1", "name": "agents"}})
	}))
	mux.HandleFunc("/v1/vaults/v1/items", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]string{{"id": "it1", "title": "GitHub"}})
	}))
	mux.HandleFunc("/v1/vaults/v1/items/it1", auth(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"fields": []map[string]any{
			{"id": "f1", "label": "token", "value": "connect-val", "purpose": ""},
			{"id": "f2", "label": "password", "value": "pw-val", "purpose": "PASSWORD"},
			{"id": "f3", "label": "token", "value": "sectioned", "section": map[string]string{"id": "s", "label": "prod"}},
		}})
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	p := mk(t, Config{Kind: "onepassword", Settings: map[string]string{"connect_url": srv.URL},
		Secrets: map[string]string{"connect_token": "connect-tok"}})
	if got, err := p.Resolve(ctx(), "op://agents/GitHub/password"); err != nil || got != "pw-val" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := p.Resolve(ctx(), "op://agents/GitHub/prod/token"); err != nil || got != "sectioned" {
		t.Fatalf("section: %q %v", got, err)
	}
	if _, err := p.Resolve(ctx(), "op://agents/GitHub/absent"); err == nil {
		t.Fatal("absent field")
	}
	bad := mk(t, Config{Kind: "onepassword", Settings: map[string]string{"connect_url": srv.URL},
		Secrets: map[string]string{"connect_token": "wrong-tok"}})
	if _, err := bad.Resolve(ctx(), "op://agents/GitHub/password"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestKDBX(t *testing.T) {
	fakeBin(t, "keepassxc-cli", `
pw=$(head -n1)
[ "$pw" = "db-pass-123" ] || { echo "Error while reading the database: Invalid credentials were provided" >&2; exit 1; }
attr=""; while [ $# -gt 0 ]; do [ "$1" = "-a" ] && attr="$2"; last="$1"; shift; done
printf '%s-of-%s\n' "$attr" "$last"`)
	db := filepath.Join(t.TempDir(), "x.kdbx")
	p := mk(t, Config{Kind: "kdbx", Settings: map[string]string{"database": db}, Secrets: map[string]string{"password": "db-pass-123"}})
	if got, err := p.Resolve(ctx(), "kdbx://Agents/GitHub#password"); err != nil || got != "Password-of-/Agents/GitHub" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := p.Resolve(ctx(), "kdbx://Agents/GitHub#My Field"); got != "My Field-of-/Agents/GitHub" {
		t.Fatalf("custom: %q", got)
	}
	bad := mk(t, Config{Kind: "kdbx", Settings: map[string]string{"database": db}, Secrets: map[string]string{"password": "nope-nope"}})
	_, err := bad.Resolve(ctx(), "kdbx://a/b")
	if !errors.Is(err, ErrUnavailable) || strings.Contains(err.Error(), "nope-nope") {
		t.Fatalf("%v", err)
	}
}

func TestVaultKV2TokenAndAppRole(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/auth/approle/login", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["role_id"] != "role-1" || in["secret_id"] != "sec-1" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]any{"client_token": "ar-token"}})
	})
	mux.HandleFunc("/v1/kv/data/apps/gh", func(w http.ResponseWriter, r *http.Request) {
		if t := r.Header.Get("X-Vault-Token"); t != "root-tok" && t != "ar-token" {
			w.WriteHeader(403)
			return
		}
		if r.Header.Get("X-Vault-Namespace") != "team" {
			w.WriteHeader(404)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]any{"token": "kv-val", "n": 5}}})
	})
	mux.HandleFunc("/v1/secret/old", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"k": "v1-val"}})
	})
	mux.HandleFunc("/v1/auth/token/lookup-self", func(w http.ResponseWriter, r *http.Request) {})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	set := map[string]string{"address": srv.URL, "namespace": "team"}

	p := mk(t, Config{Kind: "vault", Settings: set, Secrets: map[string]string{"token": "root-tok"}})
	if got, err := p.Resolve(ctx(), "vault://kv/apps/gh#token"); err != nil || got != "kv-val" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := p.Resolve(ctx(), "vault://kv/apps/gh#nokey"); err == nil {
		t.Fatal("missing key")
	}
	if _, err := p.Resolve(ctx(), "vault://kv/apps/none#x"); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("404 is not unavailability: %v", err)
	}
	ar := mk(t, Config{Kind: "vault", Settings: set, Secrets: map[string]string{"role_id": "role-1", "secret_id": "sec-1"}})
	if got, err := ar.Resolve(ctx(), "vault://kv/apps/gh#token"); err != nil || got != "kv-val" {
		t.Fatalf("approle %q %v", got, err)
	}
	bad := mk(t, Config{Kind: "vault", Settings: set, Secrets: map[string]string{"token": "stale-token"}})
	if _, err := bad.Resolve(ctx(), "vault://kv/apps/gh#token"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	v1 := mk(t, Config{Kind: "vault", Settings: map[string]string{"address": srv.URL, "kv_version": "1"}, Secrets: map[string]string{"token": "t"}})
	if got, err := v1.Resolve(ctx(), "vault://secret/old#k"); err != nil || got != "v1-val" {
		t.Fatalf("kv1 %q %v", got, err)
	}
}

func TestPass(t *testing.T) {
	fakeBin(t, "pass", `
case "$PASSWORD_STORE_GPG_OPTS" in *"pinentry-mode error"*) ;; *) echo "bad opts" >&2; exit 2;; esac
if [ "$2" = "locked/entry" ]; then echo "gpg: decryption failed: No secret key" >&2; exit 1; fi
printf 'line1-pw\nuser: bob\nurl: https://x\n'`)
	p := mk(t, Config{Kind: "pass", Settings: map[string]string{"store_dir": t.TempDir()}})
	if got, err := p.Resolve(ctx(), "pass://web/site"); err != nil || got != "line1-pw" {
		t.Fatalf("%q %v", got, err)
	}
	if got, _ := p.Resolve(ctx(), "pass://web/site#user"); got != "bob" {
		t.Fatalf("field %q", got)
	}
	if _, err := p.Resolve(ctx(), "pass://locked/entry"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
	if _, err := p.Resolve(ctx(), "pass://../etc/passwd"); err == nil {
		t.Fatal("traversal")
	}
}

func TestMissingBinaryIsUnavailable(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	p := mk(t, Config{Kind: "pass"})
	if _, err := p.Resolve(ctx(), "pass://a"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("%v", err)
	}
}

func TestConfigValidationAndRefRouting(t *testing.T) {
	if err := ValidateConfig(Config{Name: "a", Kind: "kdbx"}); err == nil {
		t.Fatal("kdbx needs database")
	}
	if err := ValidateConfig(Config{Name: "a", Kind: "pass", Settings: map[string]string{"typo": "1"}}); err == nil {
		t.Fatal("unknown setting accepted")
	}
	if err := ValidateConfig(Config{Name: "bad name", Kind: "pass"}); err == nil {
		t.Fatal("bad name accepted")
	}
	for ref, kind := range map[string]string{"bitwarden://x/password": "bitwarden", "op://a/b/c": "onepassword",
		"kdbx://a#b": "kdbx", "vault://kv/p#k": "vault", "pass://a": "pass", "bws://id": "bitwarden-sm"} {
		k, err := KindForRef(ref)
		if err != nil || k.Name != kind {
			t.Fatalf("%s -> %v %v", ref, k, err)
		}
	}
	if _, err := KindForRef("nope://x"); err == nil {
		t.Fatal("unknown scheme")
	}
	if _, err := ParseCheck("vault", "vault://kv/p"); err == nil {
		t.Fatal("vault ref without key accepted")
	}
}
