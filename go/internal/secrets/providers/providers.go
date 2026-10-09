// Package providers resolves credentials that live in an external password
// manager, so Grimoire can broker them under the same use-don't-read model as
// secrets stored in its own vault.
//
// Nothing here persists a resolved value. A Provider returns the value to the
// caller (the broker), which injects it into one outbound request and drops it.
// Unlock material — session keys, service-account tokens, a kdbx password —
// arrives in Config.Secrets, which the secrets package keeps sealed inside
// Grimoire's own encrypted vault.
//
// This package must not import its parent (the parent imports it).
package providers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// ErrUnavailable marks a provider that cannot answer right now: locked,
// unreachable, rejecting its credentials, or missing its binary. There is
// deliberately no fallback to anything else — a locked manager is an error the
// operator must see, not a reason to try some other value.
var ErrUnavailable = errors.New("provider unavailable")

// UnavailableError wraps a reason and satisfies errors.Is(err, ErrUnavailable).
type UnavailableError struct{ Reason string }

func (e *UnavailableError) Error() string   { return "provider unavailable: " + e.Reason }
func (e *UnavailableError) Is(t error) bool { return t == ErrUnavailable }

func unavailable(format string, a ...any) error {
	return &UnavailableError{Reason: fmt.Sprintf(format, a...)}
}

// Config is one configured provider. Settings are non-sensitive (addresses,
// binary paths, a database file); Secrets are the unlock material. Both are
// stored sealed, and Secrets values are never reported by any listing.
type Config struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Settings map[string]string `json:"settings,omitempty"`
	Secrets  map[string]string `json:"secrets,omitempty"`
}

// Item is a listing row from a provider that can enumerate items. It carries
// no value.
type Item struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Ref is the provider reference that resolves to the item's primary secret.
	Ref string `json:"ref"`
}

// Provider resolves references of one kind.
type Provider interface {
	// Resolve returns the secret a reference points at. The value must never
	// be logged or returned beyond the broker.
	Resolve(ctx context.Context, ref string) (string, error)
	// Test checks the provider is reachable and unlocked. The text it returns
	// describes state (never values) and is safe to print.
	Test(ctx context.Context) (string, error)
	// Close drops any in-memory session.
	Close()
}

// Lister is implemented by providers that can enumerate items for import.
type Lister interface {
	ListItems(ctx context.Context, folder string) ([]Item, error)
}

// Field describes one setting or secret a kind accepts.
type Field struct {
	Name     string
	Required bool
	Help     string
}

// Kind is the static description of a provider type.
type Kind struct {
	Name     string
	Scheme   string // the ref prefix: bitwarden://…
	Summary  string
	Settings []Field
	Secrets  []Field
	build    func(Config) (Provider, error)
}

// HTTPClient is the client REST providers use. The secrets package replaces it
// with one that dials through the broker's address guard; the default exists
// only so this package works standalone.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

var kinds = map[string]*Kind{}

func register(k *Kind) { kinds[k.Name] = k }

// Kinds lists the supported provider kinds, sorted by name.
func Kinds() []*Kind {
	out := make([]*Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// KindByName returns a kind or nil.
func KindByName(name string) *Kind { return kinds[name] }

// KindForRef picks the kind whose scheme prefixes the reference.
func KindForRef(ref string) (*Kind, error) {
	scheme, _, ok := strings.Cut(ref, "://")
	if !ok || scheme == "" {
		return nil, fmt.Errorf("reference %q has no scheme (expected e.g. bitwarden://ID/password)", ref)
	}
	for _, k := range kinds {
		if k.Scheme == scheme {
			return k, nil
		}
	}
	return nil, fmt.Errorf("no provider kind handles %q references", scheme+"://")
}

// New builds a provider from its configuration.
func New(cfg Config) (Provider, error) {
	k := kinds[cfg.Kind]
	if k == nil {
		return nil, fmt.Errorf("unknown provider kind %q", cfg.Kind)
	}
	if err := ValidateConfig(cfg); err != nil {
		return nil, err
	}
	return k.build(cfg)
}

// ValidateConfig rejects unknown keys and missing required ones, so a typo is
// caught at `provider add` rather than at the first agent call.
func ValidateConfig(cfg Config) error {
	k := kinds[cfg.Kind]
	if k == nil {
		return fmt.Errorf("unknown provider kind %q (try: %s)", cfg.Kind, kindNames())
	}
	if !validName(cfg.Name) {
		return fmt.Errorf("provider name %q must be letters, digits, '-', '_' or '.'", cfg.Name)
	}
	check := func(what string, have map[string]string, fields []Field) error {
		known := map[string]bool{}
		for _, f := range fields {
			known[f.Name] = true
		}
		for key := range have {
			if !known[key] {
				return fmt.Errorf("%s has no %s %q", cfg.Kind, what, key)
			}
		}
		for _, f := range fields {
			if f.Required && strings.TrimSpace(have[f.Name]) == "" {
				return fmt.Errorf("%s needs %s %q", cfg.Kind, what, f.Name)
			}
		}
		return nil
	}
	if err := check("setting", cfg.Settings, k.Settings); err != nil {
		return err
	}
	if err := check("secret", cfg.Secrets, k.Secrets); err != nil {
		return err
	}
	return nil
}

func kindNames() string {
	var n []string
	for _, k := range Kinds() {
		n = append(n, k.Name)
	}
	return strings.Join(n, ", ")
}

func validName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

// stripScheme removes "scheme://" and errors if the scheme is not expected.
func stripScheme(ref, scheme string) (string, error) {
	rest, ok := strings.CutPrefix(ref, scheme+"://")
	if !ok {
		return "", fmt.Errorf("reference %q is not a %s:// reference", ref, scheme)
	}
	if strings.TrimSpace(rest) == "" {
		return "", fmt.Errorf("reference %q is empty after the scheme", ref)
	}
	return rest, nil
}

// Redact replaces every occurrence of each non-trivial secret in s.
func Redact(s string, secrets ...string) string {
	for _, v := range secrets {
		if len(v) >= 4 {
			s = strings.ReplaceAll(s, v, "[redacted]")
		}
	}
	return s
}

func secretValues(cfg Config) []string {
	out := make([]string, 0, len(cfg.Secrets))
	for _, v := range cfg.Secrets {
		out = append(out, v)
	}
	return out
}

// passEnv is the only part of the parent environment a child manager CLI sees.
// The broker's own environment can hold unrelated tokens; a password-manager
// binary has no business receiving them.
var passEnv = []string{"PATH", "HOME", "USER", "LOGNAME", "LANG", "LC_ALL", "TMPDIR",
	"XDG_RUNTIME_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "GNUPGHOME", "DBUS_SESSION_BUS_ADDRESS",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY"}

// runCLI executes a manager CLI with a scrubbed environment plus extra, a
// timeout, and stderr sanitised of unlock material.
func runCLI(ctx context.Context, cfg Config, bin string, args []string, extraEnv []string, stdin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	path, err := exec.LookPath(bin)
	if err != nil {
		return "", unavailable("%s not found on PATH (set the %q setting to its full path)", bin, "bin")
	}
	cmd := exec.CommandContext(ctx, path, args...)
	var env []string
	for _, k := range passEnv {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	cmd.Env = append(env, extraEnv...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := nodeNoise(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		msg = Redact(msg, secretValues(cfg)...)
		if len(msg) > 300 {
			msg = msg[:300] + "…"
		}
		if ctx.Err() != nil {
			return "", unavailable("%s timed out", bin)
		}
		return "", &cliError{bin: bin, msg: msg}
	}
	return out.String(), nil
}

// cliError is a failed CLI call; its text is already sanitised.
type cliError struct{ bin, msg string }

func (e *cliError) Error() string { return e.bin + ": " + e.msg }

func setting(cfg Config, key, def string) string {
	if v := strings.TrimSpace(cfg.Settings[key]); v != "" {
		return v
	}
	return def
}

func trimNL(s string) string { return strings.TrimRight(s, "\r\n") }

// ParseCheck validates a reference's syntax for a kind without contacting the
// provider, so a typo is caught at `secret link` rather than at first use.
func ParseCheck(kind, ref string) (string, error) {
	k := kinds[kind]
	if k == nil {
		return "", fmt.Errorf("unknown provider kind %q", kind)
	}
	rest, err := stripScheme(ref, k.Scheme)
	if err != nil {
		return "", err
	}
	switch kind {
	case "bitwarden":
		_, _, _, err = parseBWRef(ref)
	case "onepassword":
		if n := len(strings.Split(strings.SplitN(rest, "?", 2)[0], "/")); n < 3 || n > 4 {
			err = fmt.Errorf("1Password reference must be op://vault/item/field")
		}
	case "vault":
		loc, key, _ := strings.Cut(rest, "#")
		if !strings.Contains(strings.Trim(loc, "/"), "/") || key == "" {
			err = fmt.Errorf("vault reference must be vault://mount/path#key")
		}
	case "pass", "kdbx":
		if strings.Contains(rest, "..") {
			err = fmt.Errorf("entry path must not contain '..'")
		}
	}
	return rest, err
}

// nodeNoise drops Node.js deprecation chatter (bw is a Node program) so the
// real error is what an operator sees.
func nodeNoise(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(l, "(node:") || strings.HasPrefix(l, "(Use `node") {
			continue
		}
		keep = append(keep, l)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
