package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Bitwarden Password Manager (and Vaultwarden, which speaks the same API).
//
// Two transports, one reference syntax:
//
//	bitwarden://<item-id>/password        (default field)
//	bitwarden://<item-id>/username|totp|notes|uri
//	bitwarden://<item-id>/field/<custom field name>
//
// CLI mode shells out to `bw`. The vault must be unlocked, which needs either a
// session key (`bw unlock --raw`) or the master password (plus an API key or
// the account email, so Grimoire can log in by itself). Serve mode talks to a
// loopback `bw serve`, which holds its own unlocked session.
func init() {
	register(&Kind{
		Name: "bitwarden", Scheme: "bitwarden",
		Summary: "Bitwarden / Vaultwarden password manager via the bw CLI or a local `bw serve`",
		Settings: []Field{
			{"bin", false, "path to the bw binary (default: bw)"},
			{"server", false, "self-hosted server URL, e.g. your Vaultwarden (CLI mode)"},
			{"email", false, "account email, for master-password login without an API key"},
			{"appdata_dir", false, "private bw state directory (default: ~/.config/grimoire/bw-NAME when Grimoire logs in itself)"},
			{"serve_url", false, "loopback bw serve URL, e.g. http://127.0.0.1:8087 (selects serve mode)"},
			{"cache_seconds", false, "keep resolved values in memory this long (default 0)"},
		},
		Secrets: []Field{
			{"session", false, "BW_SESSION from `bw unlock --raw` (expires when the vault locks)"},
			{"master_password", false, "master password, so Grimoire can unlock the vault itself"},
			{"client_id", false, "personal API key client_id (bw login --apikey)"},
			{"client_secret", false, "personal API key client_secret"},
		},
		build: func(cfg Config) (Provider, error) {
			if cfg.Settings["serve_url"] == "" && cfg.Secrets["session"] == "" && cfg.Secrets["master_password"] == "" {
				return nil, fmt.Errorf("bitwarden needs a session or master_password secret (or a serve_url)")
			}
			if u := cfg.Settings["serve_url"]; u != "" {
				pu, err := url.Parse(u)
				if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") {
					return nil, fmt.Errorf("serve_url %q is not an http(s) URL", u)
				}
				if !loopbackHost(pu.Hostname()) {
					return nil, fmt.Errorf("serve_url must be a loopback address: bw serve has no authentication of its own")
				}
			}
			return &bitwarden{cfg: cfg}, nil
		},
	})
}

func loopbackHost(h string) bool {
	return h == "localhost" || h == "127.0.0.1" || h == "::1" || strings.HasPrefix(h, "127.")
}

type bitwarden struct {
	cfg Config
	mu  sync.Mutex
	// session is the key obtained by unlocking with the master password. It is
	// held in memory only and dropped by Close.
	session string
}

func (b *bitwarden) Close() {
	b.mu.Lock()
	b.session = ""
	b.mu.Unlock()
}

func (b *bitwarden) bin() string { return setting(b.cfg, "bin", "bw") }

// parse splits "<id>/<field>[/<name>]".
func parseBWRef(ref string) (id, field, name string, err error) {
	rest, err := stripScheme(ref, "bitwarden")
	if err != nil {
		return "", "", "", err
	}
	parts := strings.SplitN(rest, "/", 3)
	id = parts[0]
	field = "password"
	if len(parts) > 1 && parts[1] != "" {
		field = strings.ToLower(parts[1])
	}
	if len(parts) > 2 {
		name = parts[2]
	}
	switch field {
	case "password", "username", "totp", "notes", "uri":
	case "field":
		if name == "" {
			return "", "", "", fmt.Errorf("bitwarden://ID/field/NAME needs a field name")
		}
	default:
		return "", "", "", fmt.Errorf("unknown bitwarden field %q (password, username, totp, notes, uri, field/NAME)", field)
	}
	if id == "" || strings.ContainsAny(id, " \t\n") {
		return "", "", "", fmt.Errorf("bitwarden reference has no valid item id")
	}
	return id, field, name, nil
}

func (b *bitwarden) Resolve(ctx context.Context, ref string) (string, error) {
	id, field, name, err := parseBWRef(ref)
	if err != nil {
		return "", err
	}
	if b.cfg.Settings["serve_url"] != "" {
		return b.serveResolve(ctx, id, field, name)
	}
	return b.cliResolve(ctx, id, field, name)
}

// ---- CLI mode --------------------------------------------------------------

func (b *bitwarden) appdata() string {
	if d := b.cfg.Settings["appdata_dir"]; d != "" {
		return d
	}
	// Only isolate when Grimoire has to log in or switch server itself; with a
	// caller-supplied session the user's own bw state is what that session fits.
	if b.cfg.Secrets["session"] == "" || b.cfg.Settings["server"] != "" {
		base, err := os.UserConfigDir()
		if err != nil {
			base = os.TempDir()
		}
		return filepath.Join(base, "grimoire", "bw-"+b.cfg.Name)
	}
	return ""
}

func (b *bitwarden) baseEnv() []string {
	env := []string{"BW_NOINTERACTION=true"}
	if d := b.appdata(); d != "" {
		_ = os.MkdirAll(d, 0o700)
		env = append(env, "BITWARDENCLI_APPDATA_DIR="+d)
	}
	return env
}

func (b *bitwarden) bw(ctx context.Context, extra []string, args ...string) (string, error) {
	return runCLI(ctx, b.cfg, b.bin(), args, append(b.baseEnv(), extra...), "")
}

// unlock logs in if needed and returns a session key, using the master
// password. It is the only place the master password is handed to bw, and it
// goes by environment variable, never argv (argv is world-readable in /proc).
func (b *bitwarden) unlock(ctx context.Context) (string, error) {
	pw := b.cfg.Secrets["master_password"]
	if pw == "" {
		return "", unavailable("bitwarden vault is locked and no master_password is configured")
	}
	if srv := b.cfg.Settings["server"]; srv != "" {
		if _, err := b.bw(ctx, nil, "config", "server", srv); err != nil {
			return "", unavailable("%v", err)
		}
	}
	st, _ := b.bw(ctx, nil, "status")
	status := ""
	var sj struct {
		Status string `json:"status"`
	}
	if json.Unmarshal([]byte(st), &sj) == nil {
		status = sj.Status
	}
	pwEnv := []string{"BW_PASSWORD=" + pw}
	if status == "unauthenticated" || status == "" {
		switch {
		case b.cfg.Secrets["client_id"] != "":
			_, err := b.bw(ctx, []string{"BW_CLIENTID=" + b.cfg.Secrets["client_id"],
				"BW_CLIENTSECRET=" + b.cfg.Secrets["client_secret"]}, "login", "--apikey")
			if err != nil && !strings.Contains(err.Error(), "already logged in") {
				return "", unavailable("%v", err)
			}
		case b.cfg.Settings["email"] != "":
			out, err := b.bw(ctx, pwEnv, "login", b.cfg.Settings["email"], "--passwordenv", "BW_PASSWORD", "--raw")
			if err != nil && !strings.Contains(err.Error(), "already logged in") {
				return "", unavailable("%v", err)
			}
			if s := strings.TrimSpace(out); err == nil && s != "" {
				return s, nil
			}
		default:
			return "", unavailable("bitwarden is not logged in: configure client_id/client_secret or the email setting")
		}
	}
	out, err := b.bw(ctx, pwEnv, "unlock", "--passwordenv", "BW_PASSWORD", "--raw")
	if err != nil {
		return "", unavailable("%v", err)
	}
	s := strings.TrimSpace(out)
	if s == "" {
		return "", unavailable("bw unlock returned no session")
	}
	return s, nil
}

func (b *bitwarden) sessionKey(ctx context.Context, fresh bool) (string, bool, error) {
	if s := b.cfg.Secrets["session"]; s != "" {
		return s, true, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if fresh {
		b.session = ""
	}
	if b.session == "" {
		s, err := b.unlock(ctx)
		if err != nil {
			return "", false, err
		}
		b.session = s
	}
	return b.session, false, nil
}

func lockedErr(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "locked") || strings.Contains(m, "session") ||
		strings.Contains(m, "not logged in") || strings.Contains(m, "master password")
}

func (b *bitwarden) cliResolve(ctx context.Context, id, field, name string) (string, error) {
	var args []string
	if field == "field" {
		args = []string{"get", "item", id, "--raw"}
	} else {
		args = []string{"get", field, id, "--raw"}
	}
	run := func(fresh bool) (string, bool, error) {
		sess, static, err := b.sessionKey(ctx, fresh)
		if err != nil {
			return "", static, err
		}
		out, err := b.bw(ctx, []string{"BW_SESSION=" + sess}, args...)
		return out, static, err
	}
	out, static, err := run(false)
	if err != nil && !static && lockedErr(err) {
		out, static, err = run(true) // the session lapsed; unlock once more
	}
	if err != nil {
		var u *UnavailableError
		if asUnavail(err, &u) {
			return "", err
		}
		if lockedErr(err) {
			if static {
				return "", unavailable("bitwarden rejected the stored session key (vault locked or key expired): %v", err)
			}
			return "", unavailable("%v", err)
		}
		return "", err
	}
	if field == "field" {
		return customField([]byte(out), name)
	}
	return trimNL(out), nil
}

func asUnavail(err error, target **UnavailableError) bool {
	u, ok := err.(*UnavailableError)
	if ok {
		*target = u
	}
	return ok
}

func customField(itemJSON []byte, name string) (string, error) {
	var it struct {
		Fields []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(itemJSON, &it); err != nil {
		return "", fmt.Errorf("bitwarden returned an unreadable item")
	}
	for _, f := range it.Fields {
		if strings.EqualFold(f.Name, name) {
			return f.Value, nil
		}
	}
	return "", fmt.Errorf("bitwarden item has no custom field %q", name)
}

// ---- serve mode ------------------------------------------------------------

func (b *bitwarden) serve(ctx context.Context, method, path string, body any) (json.RawMessage, int, error) {
	var rdr io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(b.cfg.Settings["serve_url"], "/")+path, rdr)
	if err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, 0, unavailable("bw serve unreachable: %v", Redact(err.Error(), secretValues(b.cfg)...))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env struct {
		Success bool            `json:"success"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil, resp.StatusCode, fmt.Errorf("bw serve answered %d with an unreadable body", resp.StatusCode)
	}
	if resp.StatusCode >= 400 || !env.Success {
		msg := env.Message
		if msg == "" {
			msg = fmt.Sprintf("status %d", resp.StatusCode)
		}
		return nil, resp.StatusCode, fmt.Errorf("bw serve: %s", Redact(msg, secretValues(b.cfg)...))
	}
	return env.Data, resp.StatusCode, nil
}

func (b *bitwarden) serveUnlocked(ctx context.Context) error {
	data, _, err := b.serve(ctx, "GET", "/status", nil)
	if err != nil {
		return err
	}
	var st struct {
		Template struct {
			Status string `json:"status"`
		} `json:"template"`
	}
	_ = json.Unmarshal(data, &st)
	if st.Template.Status == "unlocked" {
		return nil
	}
	pw := b.cfg.Secrets["master_password"]
	if pw == "" {
		return unavailable("bw serve is %s and no master_password is configured", orStr(st.Template.Status, "locked"))
	}
	if _, _, err := b.serve(ctx, "POST", "/unlock", map[string]string{"password": pw}); err != nil {
		return unavailable("%v", err)
	}
	return nil
}

func orStr(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (b *bitwarden) serveResolve(ctx context.Context, id, field, name string) (string, error) {
	if err := b.serveUnlocked(ctx); err != nil {
		return "", err
	}
	if field == "field" {
		data, _, err := b.serve(ctx, "GET", "/object/item/"+url.PathEscape(id), nil)
		if err != nil {
			return "", err
		}
		return customField(data, name)
	}
	data, _, err := b.serve(ctx, "GET", "/object/"+field+"/"+url.PathEscape(id), nil)
	if err != nil {
		return "", err
	}
	var s struct {
		Data string `json:"data"`
	}
	if json.Unmarshal(data, &s) == nil && s.Data != "" {
		return s.Data, nil
	}
	var plain string
	if json.Unmarshal(data, &plain) == nil && plain != "" {
		return plain, nil
	}
	return "", fmt.Errorf("bw serve returned no %s for that item", field)
}

// ---- Test / listing --------------------------------------------------------

func (b *bitwarden) Test(ctx context.Context) (string, error) {
	if b.cfg.Settings["serve_url"] != "" {
		if err := b.serveUnlocked(ctx); err != nil {
			return "", err
		}
		return "bw serve reachable and unlocked", nil
	}
	sess, _, err := b.sessionKey(ctx, false)
	if err != nil {
		return "", err
	}
	if _, err := b.bw(ctx, []string{"BW_SESSION=" + sess}, "list", "folders", "--raw"); err != nil {
		return "", unavailable("%v", err)
	}
	return "bw vault unlocked", nil
}

type bwRow struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	FolderID string `json:"folderId"`
}

func (b *bitwarden) listJSON(ctx context.Context, kind string, extra ...string) ([]bwRow, error) {
	var raw []byte
	if b.cfg.Settings["serve_url"] != "" {
		if err := b.serveUnlocked(ctx); err != nil {
			return nil, err
		}
		q := url.Values{}
		for i := 0; i+1 < len(extra); i += 2 {
			q.Set(strings.TrimPrefix(extra[i], "--"), extra[i+1])
		}
		data, _, err := b.serve(ctx, "GET", "/list/object/"+kind+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		var wrap struct {
			Data []json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &wrap) == nil && wrap.Data != nil {
			raw, _ = json.Marshal(wrap.Data)
		} else {
			raw = data
		}
	} else {
		sess, _, err := b.sessionKey(ctx, false)
		if err != nil {
			return nil, err
		}
		args := append([]string{"list", kind, "--raw"}, extra...)
		out, err := b.bw(ctx, []string{"BW_SESSION=" + sess}, args...)
		if err != nil {
			return nil, unavailable("%v", err)
		}
		raw = []byte(out)
	}
	var rows []bwRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bitwarden returned an unreadable list")
	}
	return rows, nil
}

// ListItems lists items in the named folder (matched by exact name). It
// returns names and ids only.
func (b *bitwarden) ListItems(ctx context.Context, folder string) ([]Item, error) {
	folders, err := b.listJSON(ctx, "folders", "--search", folder)
	if err != nil {
		return nil, err
	}
	folderID := ""
	for _, f := range folders {
		if f.Name == folder {
			folderID = f.ID
		}
	}
	if folderID == "" {
		return nil, fmt.Errorf("no bitwarden folder named %q", folder)
	}
	items, err := b.listJSON(ctx, "items", "--folderid", folderID)
	if err != nil {
		return nil, err
	}
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, Item{ID: it.ID, Name: it.Name, Ref: "bitwarden://" + it.ID + "/password"})
	}
	return out, nil
}
