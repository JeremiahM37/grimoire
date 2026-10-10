package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

func init() {
	// ---- Bitwarden Secrets Manager (bws CLI) ----
	register(&Kind{
		Name: "bitwarden-sm", Scheme: "bws",
		Summary: "Bitwarden Secrets Manager via the bws CLI and a machine-account access token",
		Settings: []Field{
			{"bin", false, "path to the bws binary (default: bws)"},
			{"server_url", false, "self-hosted server base URL (--server-url)"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		Secrets: []Field{{"access_token", true, "machine-account access token"}},
		build:   func(cfg Config) (Provider, error) { return &bws{cfg: cfg}, nil },
	})
	// ---- 1Password ----
	register(&Kind{
		Name: "onepassword", Scheme: "op",
		Summary: "1Password via a service account (op CLI) or a Connect server (REST)",
		Settings: []Field{
			{"bin", false, "path to the op binary (default: op)"},
			{"connect_url", false, "1Password Connect server URL (selects Connect mode)"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		Secrets: []Field{
			{"service_account_token", false, "service account token (op CLI mode)"},
			{"connect_token", false, "Connect server access token (Connect mode)"},
		},
		build: func(cfg Config) (Provider, error) {
			if cfg.Settings["connect_url"] != "" {
				if cfg.Secrets["connect_token"] == "" {
					return nil, fmt.Errorf("onepassword connect mode needs the connect_token secret")
				}
			} else if cfg.Secrets["service_account_token"] == "" {
				return nil, fmt.Errorf("onepassword needs the service_account_token secret (or connect_url + connect_token)")
			}
			return &onepassword{cfg: cfg}, nil
		},
	})
	// ---- KeePass / KeePassXC ----
	register(&Kind{
		Name: "kdbx", Scheme: "kdbx",
		Summary: "KeePass/KeePassXC .kdbx database via keepassxc-cli",
		Settings: []Field{
			{"database", true, "path to the .kdbx file"},
			{"key_file", false, "path to a key file, if the database uses one"},
			{"bin", false, "path to keepassxc-cli (default: keepassxc-cli)"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		Secrets: []Field{{"password", false, "database master password (omit for key-file-only databases)"}},
		build:   func(cfg Config) (Provider, error) { return &kdbx{cfg: cfg}, nil },
	})
	// ---- HashiCorp Vault / OpenBao ----
	register(&Kind{
		Name: "vault", Scheme: "vault",
		Summary: "HashiCorp Vault / OpenBao KV secrets over REST (token or AppRole)",
		Settings: []Field{
			{"address", true, "server address, e.g. https://vault.example:8200"},
			{"namespace", false, "Vault Enterprise namespace"},
			{"kv_version", false, "KV engine version, 1 or 2 (default 2)"},
			{"approle_mount", false, "AppRole auth mount (default approle)"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		Secrets: []Field{
			{"token", false, "Vault token"},
			{"role_id", false, "AppRole role_id"},
			{"secret_id", false, "AppRole secret_id"},
		},
		build: func(cfg Config) (Provider, error) {
			if cfg.Secrets["token"] == "" && (cfg.Secrets["role_id"] == "" || cfg.Secrets["secret_id"] == "") {
				return nil, fmt.Errorf("vault needs a token secret, or both role_id and secret_id")
			}
			if v := cfg.Settings["kv_version"]; v != "" && v != "1" && v != "2" {
				return nil, fmt.Errorf("kv_version must be 1 or 2")
			}
			u, err := url.Parse(cfg.Settings["address"])
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				return nil, fmt.Errorf("address %q is not an http(s) URL", cfg.Settings["address"])
			}
			return &hcVault{cfg: cfg}, nil
		},
	})
	// ---- pass ----
	register(&Kind{
		Name: "pass", Scheme: "pass",
		Summary: "pass (the standard unix password manager, gpg) — needs gpg-agent already unlocked",
		Settings: []Field{
			{"bin", false, "path to pass (default: pass)"},
			{"store_dir", false, "PASSWORD_STORE_DIR"},
			{"gnupg_home", false, "GNUPGHOME"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		build: func(cfg Config) (Provider, error) { return &passP{cfg: cfg}, nil },
	})
}

// ---- bws --------------------------------------------------------------------

type bws struct{ cfg Config }

func (b *bws) Close() {}

func (b *bws) run(ctx context.Context, args ...string) (string, error) {
	if s := b.cfg.Settings["server_url"]; s != "" {
		args = append([]string{"--server-url", s}, args...)
	}
	out, err := runCLI(ctx, b.cfg, setting(b.cfg, "bin", "bws"), args,
		[]string{"BWS_ACCESS_TOKEN=" + b.cfg.Secrets["access_token"]}, "")
	if _, ok := err.(*cliError); ok {
		return "", unavailable("%v", err)
	}
	return out, err
}

// Resolve: bws://<secret-id> yields the secret's value; bws://<id>/note its note.
func (b *bws) Resolve(ctx context.Context, ref string) (string, error) {
	rest, err := stripScheme(ref, "bws")
	if err != nil {
		return "", err
	}
	id, field, _ := strings.Cut(rest, "/")
	if field == "" {
		field = "value"
	}
	if field != "value" && field != "note" && field != "key" {
		return "", fmt.Errorf("bws field %q unknown (value, note, key)", field)
	}
	out, err := b.run(ctx, "secret", "get", id, "--output", "json")
	if err != nil {
		return "", err
	}
	var s map[string]any
	if json.Unmarshal([]byte(out), &s) != nil {
		return "", fmt.Errorf("bws returned unreadable output")
	}
	v, _ := s[field].(string)
	if v == "" {
		return "", fmt.Errorf("bws secret has no %s", field)
	}
	return v, nil
}

func (b *bws) Test(ctx context.Context) (string, error) {
	if _, err := b.run(ctx, "project", "list", "--output", "json"); err != nil {
		return "", err
	}
	return "machine account token accepted", nil
}

// ---- 1Password --------------------------------------------------------------

type onepassword struct{ cfg Config }

func (o *onepassword) Close() {}

func (o *onepassword) Resolve(ctx context.Context, ref string) (string, error) {
	rest, err := stripScheme(ref, "op")
	if err != nil {
		return "", err
	}
	parts := strings.Split(strings.SplitN(rest, "?", 2)[0], "/")
	if len(parts) < 3 || len(parts) > 4 {
		return "", fmt.Errorf("1Password reference must be op://vault/item/field or op://vault/item/section/field")
	}
	if o.cfg.Settings["connect_url"] != "" {
		return o.connectResolve(ctx, parts)
	}
	out, err := runCLI(ctx, o.cfg, setting(o.cfg, "bin", "op"), []string{"read", "--no-newline", ref},
		[]string{"OP_SERVICE_ACCOUNT_TOKEN=" + o.cfg.Secrets["service_account_token"]}, "")
	if err != nil {
		if _, ok := err.(*cliError); ok {
			return "", unavailable("%v", err)
		}
		return "", err
	}
	return trimNL(out), nil
}

func (o *onepassword) connect(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(o.cfg.Settings["connect_url"], "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+o.cfg.Secrets["connect_token"])
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return unavailable("1Password Connect unreachable: %v", Redact(err.Error(), secretValues(o.cfg)...))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return unavailable("1Password Connect rejected the token (%d)", resp.StatusCode)
	case resp.StatusCode == 404:
		return fmt.Errorf("1Password Connect: not found")
	case resp.StatusCode >= 400:
		return fmt.Errorf("1Password Connect answered %d", resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

func (o *onepassword) connectResolve(ctx context.Context, parts []string) (string, error) {
	vaultName, itemName, field := parts[0], parts[1], parts[len(parts)-1]
	section := ""
	if len(parts) == 4 {
		section = parts[2]
	}
	filt := func(k, v string) string {
		return "?filter=" + url.QueryEscape(fmt.Sprintf(`%s eq "%s"`, k, strings.ReplaceAll(v, `"`, ``)))
	}
	var vaults []struct{ ID, Name string }
	if err := o.connect(ctx, "/v1/vaults"+filt("name", vaultName), &vaults); err != nil {
		return "", err
	}
	if len(vaults) == 0 { // maybe given as an id
		var v struct{ ID, Name string }
		if err := o.connect(ctx, "/v1/vaults/"+url.PathEscape(vaultName), &v); err != nil || v.ID == "" {
			return "", fmt.Errorf("1Password vault %q not found", vaultName)
		}
		vaults = append(vaults, v)
	}
	var items []struct{ ID, Title string }
	if err := o.connect(ctx, "/v1/vaults/"+vaults[0].ID+"/items"+filt("title", itemName), &items); err != nil {
		return "", err
	}
	itemID := itemName
	if len(items) > 0 {
		itemID = items[0].ID
	}
	var item struct {
		Fields []struct {
			ID, Label, Value, Purpose string
			Section                   *struct{ ID, Label string }
		}
	}
	if err := o.connect(ctx, "/v1/vaults/"+vaults[0].ID+"/items/"+url.PathEscape(itemID), &item); err != nil {
		return "", err
	}
	for _, f := range item.Fields {
		if !(strings.EqualFold(f.Label, field) || strings.EqualFold(f.ID, field) || strings.EqualFold(f.Purpose, field)) {
			continue
		}
		if section != "" && (f.Section == nil ||
			!(strings.EqualFold(f.Section.Label, section) || strings.EqualFold(f.Section.ID, section))) {
			continue
		}
		return f.Value, nil
	}
	return "", fmt.Errorf("1Password item has no field %q", field)
}

func (o *onepassword) Test(ctx context.Context) (string, error) {
	if o.cfg.Settings["connect_url"] != "" {
		var v []struct{ ID string }
		if err := o.connect(ctx, "/v1/vaults", &v); err != nil {
			return "", err
		}
		return fmt.Sprintf("Connect token accepted; %d vault(s) visible", len(v)), nil
	}
	if _, err := runCLI(ctx, o.cfg, setting(o.cfg, "bin", "op"), []string{"vault", "list", "--format", "json"},
		[]string{"OP_SERVICE_ACCOUNT_TOKEN=" + o.cfg.Secrets["service_account_token"]}, ""); err != nil {
		return "", unavailable("%v", err)
	}
	return "service account token accepted", nil
}

// ---- KeePass ----------------------------------------------------------------

type kdbx struct{ cfg Config }

func (k *kdbx) Close() {}

var kdbxAttr = map[string]string{"password": "Password", "username": "UserName", "user": "UserName",
	"url": "URL", "notes": "Notes", "title": "Title"}

func (k *kdbx) show(ctx context.Context, entry, attr string) (string, error) {
	args := []string{"show", "-q", "-s", "-a", attr}
	if kf := k.cfg.Settings["key_file"]; kf != "" {
		args = append(args, "-k", kf)
	}
	if k.cfg.Secrets["password"] == "" {
		args = append(args, "--no-password")
	}
	args = append(args, k.cfg.Settings["database"], entry)
	out, err := runCLI(ctx, k.cfg, setting(k.cfg, "bin", "keepassxc-cli"), args, nil, k.cfg.Secrets["password"]+"\n")
	if err != nil {
		if ce, ok := err.(*cliError); ok {
			return "", unavailable("%v", ce)
		}
		return "", err
	}
	return trimNL(out), nil
}

// Resolve: kdbx://Group/Entry#Field (field defaults to Password).
func (k *kdbx) Resolve(ctx context.Context, ref string) (string, error) {
	rest, err := stripScheme(ref, "kdbx")
	if err != nil {
		return "", err
	}
	entry, field, _ := strings.Cut(rest, "#")
	attr := "Password"
	if field != "" {
		attr = field
		if m, ok := kdbxAttr[strings.ToLower(field)]; ok {
			attr = m
		}
	}
	return k.show(ctx, "/"+strings.TrimPrefix(entry, "/"), attr)
}

func (k *kdbx) Test(ctx context.Context) (string, error) {
	args := []string{"ls", "-q"}
	if kf := k.cfg.Settings["key_file"]; kf != "" {
		args = append(args, "-k", kf)
	}
	if k.cfg.Secrets["password"] == "" {
		args = append(args, "--no-password")
	}
	args = append(args, k.cfg.Settings["database"])
	if _, err := runCLI(ctx, k.cfg, setting(k.cfg, "bin", "keepassxc-cli"), args, nil, k.cfg.Secrets["password"]+"\n"); err != nil {
		return "", unavailable("%v", err)
	}
	return "database opens", nil
}

// ---- Vault / OpenBao ---------------------------------------------------------

type hcVault struct {
	cfg   Config
	mu    sync.Mutex
	token string // AppRole login result, memory only
}

func (h *hcVault) Close() { h.mu.Lock(); h.token = ""; h.mu.Unlock() }

func (h *hcVault) do(ctx context.Context, method, path, token string, body any) (map[string]any, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(h.cfg.Settings["address"], "/")+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if ns := h.cfg.Settings["namespace"]; ns != "" {
		req.Header.Set("X-Vault-Namespace", ns)
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, unavailable("vault unreachable: %v", Redact(err.Error(), secretValues(h.cfg)...))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m, resp.StatusCode, nil
}

func (h *hcVault) clientToken(ctx context.Context, fresh bool) (string, error) {
	if t := h.cfg.Secrets["token"]; t != "" {
		return t, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if fresh {
		h.token = ""
	}
	if h.token != "" {
		return h.token, nil
	}
	m, code, err := h.do(ctx, "POST", "/v1/auth/"+setting(h.cfg, "approle_mount", "approle")+"/login", "",
		map[string]string{"role_id": h.cfg.Secrets["role_id"], "secret_id": h.cfg.Secrets["secret_id"]})
	if err != nil {
		return "", err
	}
	auth, _ := m["auth"].(map[string]any)
	t, _ := auth["client_token"].(string)
	if code >= 400 || t == "" {
		return "", unavailable("vault AppRole login failed (%d)", code)
	}
	h.token = t
	return t, nil
}

// Resolve: vault://<mount>/<path>#<key>.
func (h *hcVault) Resolve(ctx context.Context, ref string) (string, error) {
	rest, err := stripScheme(ref, "vault")
	if err != nil {
		return "", err
	}
	loc, key, _ := strings.Cut(rest, "#")
	mount, p, ok := strings.Cut(strings.Trim(loc, "/"), "/")
	if !ok || key == "" {
		return "", fmt.Errorf("vault reference must be vault://mount/path#key")
	}
	api := "/v1/" + mount + "/data/" + p
	if h.cfg.Settings["kv_version"] == "1" {
		api = "/v1/" + mount + "/" + p
	}
	get := func(fresh bool) (map[string]any, int, error) {
		t, err := h.clientToken(ctx, fresh)
		if err != nil {
			return nil, 0, err
		}
		return h.do(ctx, "GET", api, t, nil)
	}
	m, code, err := get(false)
	if err == nil && code == 403 && h.cfg.Secrets["token"] == "" {
		m, code, err = get(true)
	}
	if err != nil {
		return "", err
	}
	switch {
	case code == 403 || code == 401:
		return "", unavailable("vault denied the token (%d): expired, sealed, or lacking policy", code)
	case code == 503:
		return "", unavailable("vault is sealed or unavailable")
	case code == 404:
		return "", fmt.Errorf("vault: no secret at %s/%s", mount, p)
	case code >= 400:
		return "", fmt.Errorf("vault answered %d", code)
	}
	data, _ := m["data"].(map[string]any)
	if h.cfg.Settings["kv_version"] != "1" {
		data, _ = data["data"].(map[string]any)
	}
	v, ok := data[key]
	if !ok {
		return "", fmt.Errorf("vault secret has no key %q", key)
	}
	if s, ok := v.(string); ok {
		return s, nil
	}
	raw, _ := json.Marshal(v)
	return string(raw), nil
}

func (h *hcVault) Test(ctx context.Context) (string, error) {
	t, err := h.clientToken(ctx, false)
	if err != nil {
		return "", err
	}
	_, code, err := h.do(ctx, "GET", "/v1/auth/token/lookup-self", t, nil)
	if err != nil {
		return "", err
	}
	if code != 200 {
		return "", unavailable("vault rejected the token (%d)", code)
	}
	return "vault token valid", nil
}

// ---- pass -------------------------------------------------------------------

type passP struct{ cfg Config }

func (p *passP) Close() {}

func (p *passP) env() []string {
	// --pinentry-mode error: a locked key fails at once instead of trying to
	// open a prompt nobody is there to answer.
	env := []string{"PASSWORD_STORE_GPG_OPTS=--batch --pinentry-mode error"}
	if d := p.cfg.Settings["store_dir"]; d != "" {
		env = append(env, "PASSWORD_STORE_DIR="+d)
	}
	if d := p.cfg.Settings["gnupg_home"]; d != "" {
		env = append(env, "GNUPGHOME="+d)
	}
	return env
}

// Resolve: pass://path/to/entry (first line) or pass://path/to/entry#field
// for a "field: value" line.
func (p *passP) Resolve(ctx context.Context, ref string) (string, error) {
	rest, err := stripScheme(ref, "pass")
	if err != nil {
		return "", err
	}
	entry, field, _ := strings.Cut(rest, "#")
	if strings.Contains(entry, "..") {
		return "", fmt.Errorf("pass entry must not contain '..'")
	}
	out, err := runCLI(ctx, p.cfg, setting(p.cfg, "bin", "pass"), []string{"show", entry}, p.env(), "")
	if err != nil {
		if ce, ok := err.(*cliError); ok {
			return "", unavailable("%v", ce)
		}
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n")
	if field == "" || strings.EqualFold(field, "password") {
		return lines[0], nil
	}
	for _, l := range lines[1:] {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.EqualFold(strings.TrimSpace(k), field) {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("pass entry has no %q line", field)
}

func (p *passP) Test(ctx context.Context) (string, error) {
	if _, err := runCLI(ctx, p.cfg, setting(p.cfg, "bin", "pass"), []string{"ls"}, p.env(), ""); err != nil {
		return "", unavailable("%v", err)
	}
	return "password store readable (entries decrypt only when used; gpg-agent must hold the key)", nil
}
