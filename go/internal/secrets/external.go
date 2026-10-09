package secrets

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/crypto"
	"github.com/JeremiahM37/grimoire/go/internal/secrets/providers"
)

// Secrets that live in an external password manager.
//
// A handle can point at an item in Bitwarden, 1Password, KeePass, Vault or
// pass instead of holding a value. Everything downstream — grants, scopes,
// the audit log, request_credential approval — works on the HANDLE and so is
// unchanged. Only Vault.Get differs: it asks the provider for the value at use
// time. The value is held in memory for at most the provider's cache window
// (default zero), is never written to disk, and is not part of any listing.
//
// Unlock material for each provider is stored in the vault's own sealed blob
// (Providers), under the same key as the secrets themselves.

// Link locates an external item.
type Link struct {
	Provider string `json:"provider"`
	Ref      string `json:"ref"`
}

// ProviderInfo describes a configured provider without any secret value.
type ProviderInfo struct {
	Name     string            `json:"name"`
	Kind     string            `json:"kind"`
	Settings map[string]string `json:"settings,omitempty"`
	// SecretKeys names the unlock material held, never the material.
	SecretKeys []string `json:"secret_keys,omitempty"`
	Links      int      `json:"links"`
}

type cacheEntry struct {
	value string
	exp   time.Time
}

// external is the live side: built provider instances and the short cache.
type external struct {
	mu    sync.Mutex
	live  map[string]providers.Provider
	built map[string]string // name -> fingerprint of the config the instance was built from
	cache map[string]cacheEntry
}

func init() {
	// REST providers dial through the same address guard the broker uses.
	// Private ranges are allowed: a Vault or Connect server on the LAN is the
	// normal case, and link-local/metadata stay refused regardless.
	providers.HTTPClient = &http.Client{Timeout: 30 * time.Second, Transport: guardedTransport(true)}
}

// reset drops every session and cached value. Called on lock and idle lock.
func (e *external) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, p := range e.live {
		p.Close()
	}
	e.live, e.built, e.cache = nil, nil, nil
}

func (e *external) forget(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if p, ok := e.live[name]; ok {
		p.Close()
		delete(e.live, name)
		delete(e.built, name)
	}
	for k := range e.cache {
		if strings.HasPrefix(k, name+"\x00") {
			delete(e.cache, k)
		}
	}
}

func fingerprint(cfg providers.Config) string {
	raw, _ := json.Marshal(cfg)
	return string(raw)
}

func cacheTTL(cfg providers.Config) time.Duration {
	s := cfg.Settings["cache_seconds"]
	if s == "" {
		s = os.Getenv("GRIMOIRE_PROVIDER_CACHE_SECONDS")
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

func (e *external) instance(cfg providers.Config) (providers.Provider, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.live == nil {
		e.live, e.built = map[string]providers.Provider{}, map[string]string{}
	}
	fp := fingerprint(cfg)
	if p, ok := e.live[cfg.Name]; ok && e.built[cfg.Name] == fp {
		return p, nil
	}
	if old, ok := e.live[cfg.Name]; ok {
		old.Close()
	}
	p, err := providers.New(cfg)
	if err != nil {
		return nil, err
	}
	e.live[cfg.Name], e.built[cfg.Name] = p, fp
	return p, nil
}

// resolve fetches a linked value, through the cache when one is configured.
func (e *external) resolve(cfg providers.Config, link Link) (string, error) {
	key := cfg.Name + "\x00" + link.Ref
	ttl := cacheTTL(cfg)
	if ttl > 0 {
		e.mu.Lock()
		c, ok := e.cache[key]
		e.mu.Unlock()
		if ok && Now().Before(c.exp) {
			return c.value, nil
		}
	}
	p, err := e.instance(cfg)
	if err != nil {
		return "", scrub(err, cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	val, err := p.Resolve(ctx, link.Ref)
	if err != nil {
		return "", fmt.Errorf("%s (%s): %w", link.Provider, link.Ref, scrub(err, cfg))
	}
	if ttl > 0 {
		e.mu.Lock()
		if e.cache == nil {
			e.cache = map[string]cacheEntry{}
		}
		e.cache[key] = cacheEntry{value: val, exp: Now().Add(ttl)}
		e.mu.Unlock()
	}
	return val, nil
}

// scrub keeps unlock material out of any error text, preserving its type so
// errors.Is(err, providers.ErrUnavailable) still works.
func scrub(err error, cfg providers.Config) error {
	if err == nil {
		return nil
	}
	clean := providers.Redact(err.Error(), vals(cfg.Secrets)...)
	if clean == err.Error() {
		return err
	}
	if errors.Is(err, providers.ErrUnavailable) {
		return &providers.UnavailableError{Reason: strings.TrimPrefix(clean, "provider unavailable: ")}
	}
	return errors.New(clean)
}

func vals(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// ---- sealed provider store ---------------------------------------------------

func (v *Vault) providersLocked() (map[string]providers.Config, error) {
	if !v.unlockedLocked() {
		return nil, ErrLocked
	}
	b := v.loadBlob()
	out := map[string]providers.Config{}
	if b.Providers == "" {
		return out, nil
	}
	raw, err := base64.StdEncoding.DecodeString(b.Providers)
	if err != nil {
		return nil, err
	}
	plain, err := crypto.Unseal(v.key, raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(plain, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (v *Vault) writeProvidersLocked(m map[string]providers.Config) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	sealed, err := crypto.Seal(v.key, raw)
	if err != nil {
		return err
	}
	b := v.loadBlob()
	b.Providers = base64.StdEncoding.EncodeToString(sealed)
	return v.saveBlob(b)
}

func (v *Vault) providerConfigLocked(name string) (providers.Config, error) {
	m, err := v.providersLocked()
	if err != nil {
		return providers.Config{}, err
	}
	cfg, ok := m[name]
	if !ok {
		return providers.Config{}, fmt.Errorf("provider %q is not configured (it was removed?)", name)
	}
	return cfg, nil
}

// AddProvider stores (or replaces) a provider and its unlock material.
func (v *Vault) AddProvider(cfg providers.Config) error {
	if err := providers.ValidateConfig(cfg); err != nil {
		return err
	}
	if _, err := providers.New(cfg); err != nil { // kind-specific checks
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.providersLocked()
	if err != nil {
		return err
	}
	m[cfg.Name] = cfg
	if err := v.writeProvidersLocked(m); err != nil {
		return err
	}
	v.ext.forget(cfg.Name)
	return nil
}

// RemoveProvider deletes a provider. It refuses while handles still point at
// it, so a removal cannot silently orphan secrets an agent holds grants for.
func (v *Vault) RemoveProvider(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.providersLocked()
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return fmt.Errorf("no such provider: %s", name)
	}
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	var users []string
	for n, e := range payload {
		if e.Link != nil && e.Link.Provider == name {
			users = append(users, n)
		}
	}
	if len(users) > 0 {
		sort.Strings(users)
		return fmt.Errorf("provider %s is still linked by: %s (unlink them first)", name, strings.Join(users, ", "))
	}
	delete(m, name)
	if err := v.writeProvidersLocked(m); err != nil {
		return err
	}
	v.ext.forget(name)
	return nil
}

// Providers lists configured providers without any unlock material.
func (v *Vault) Providers() ([]ProviderInfo, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.providersLocked()
	if err != nil {
		return nil, err
	}
	payload, err := v.payloadLocked()
	if err != nil {
		return nil, err
	}
	links := map[string]int{}
	for _, e := range payload {
		if e.Link != nil {
			links[e.Link.Provider]++
		}
	}
	out := make([]ProviderInfo, 0, len(m))
	for _, cfg := range m {
		pi := ProviderInfo{Name: cfg.Name, Kind: cfg.Kind, Settings: cfg.Settings, Links: links[cfg.Name]}
		for k := range cfg.Secrets {
			pi.SecretKeys = append(pi.SecretKeys, k)
		}
		sort.Strings(pi.SecretKeys)
		out = append(out, pi)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// TestProvider checks a provider is reachable and unlocked.
func (v *Vault) TestProvider(name string) (string, error) {
	v.mu.Lock()
	cfg, err := v.providerConfigLocked(name)
	v.mu.Unlock()
	if err != nil {
		return "", err
	}
	p, err := v.ext.instance(cfg)
	if err != nil {
		return "", scrub(err, cfg)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	msg, err := p.Test(ctx)
	return msg, scrub(err, cfg)
}

// ResolveProviderName picks the provider for a reference: the named one, or
// the only configured provider of the reference's kind.
func (v *Vault) ResolveProviderName(ref, explicit string) (string, error) {
	kind, err := providers.KindForRef(ref)
	if err != nil {
		return "", err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.providersLocked()
	if err != nil {
		return "", err
	}
	if explicit != "" {
		cfg, ok := m[explicit]
		if !ok {
			return "", fmt.Errorf("no such provider: %s", explicit)
		}
		if cfg.Kind != kind.Name {
			return "", fmt.Errorf("provider %s is %s, but %s:// references need %s", explicit, cfg.Kind, kind.Scheme, kind.Name)
		}
		return explicit, nil
	}
	var match []string
	for n, cfg := range m {
		if cfg.Kind == kind.Name {
			match = append(match, n)
		}
	}
	sort.Strings(match)
	switch len(match) {
	case 0:
		return "", fmt.Errorf("no %s provider configured: `grimoire secret provider add NAME --kind %s`", kind.Name, kind.Name)
	case 1:
		return match[0], nil
	}
	return "", fmt.Errorf("several %s providers (%s); pick one with --provider", kind.Name, strings.Join(match, ", "))
}

// Link makes handle point at an external item. It stores no value.
func (v *Vault) Link(handle, providerName, ref, note string) error {
	if strings.TrimSpace(handle) == "" {
		return errors.New("name required")
	}
	if k, err := providers.KindForRef(ref); err != nil {
		return err
	} else if _, err := providers.ParseCheck(k.Name, ref); err != nil {
		return err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	m, err := v.providersLocked()
	if err != nil {
		return err
	}
	if _, ok := m[providerName]; !ok {
		return fmt.Errorf("no such provider: %s", providerName)
	}
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	entry, existed := payload[handle]
	if existed && entry.Link == nil {
		return fmt.Errorf("%s already holds a stored value; `grimoire secret rm %s` first if you mean to replace it", handle, handle)
	}
	now := Now().UTC().Format(time.RFC3339)
	meta := entry.Meta
	if meta == nil {
		meta = map[string]any{MetaCreated: now}
	}
	meta[MetaUpdated] = now
	if note != "" {
		meta[MetaNote] = note
	}
	payload[handle] = secretEntry{Meta: meta, Link: &Link{Provider: providerName, Ref: ref}}
	return v.writePayloadLocked(payload)
}

// Unlink removes the pointer and the handle with it.
func (v *Vault) Unlink(handle string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	e, ok := payload[handle]
	if !ok || e.Link == nil {
		return fmt.Errorf("%s is not a linked secret", handle)
	}
	delete(payload, handle)
	return v.writePayloadLocked(payload)
}

// LinkOf reports where a handle points, or nil for a stored secret or none.
func (v *Vault) LinkOf(name string) *Link {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return nil
	}
	if e, ok := payload[name]; ok && e.Link != nil {
		l := *e.Link
		return &l
	}
	return nil
}

// Exists reports whether a handle exists, without resolving it. Issuing a
// grant must not require the external manager to be unlocked at that moment.
func (v *Vault) Exists(name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	payload, err := v.payloadLocked()
	if err != nil {
		return err
	}
	if _, ok := payload[name]; !ok {
		return fmt.Errorf("no such secret: %s", name)
	}
	return nil
}

// ListProviderItems enumerates items for import; only some providers can.
func (v *Vault) ListProviderItems(name, folder string) ([]providers.Item, error) {
	v.mu.Lock()
	cfg, err := v.providerConfigLocked(name)
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	p, err := v.ext.instance(cfg)
	if err != nil {
		return nil, scrub(err, cfg)
	}
	l, ok := p.(providers.Lister)
	if !ok {
		return nil, fmt.Errorf("%s providers cannot list items for import", cfg.Kind)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	items, err := l.ListItems(ctx, folder)
	return items, scrub(err, cfg)
}

// ResolveRef resolves a reference through a named provider without a handle.
// It exists for the operator-run import; no API or MCP route calls it.
func (v *Vault) ResolveRef(providerName, ref string) (string, error) {
	v.mu.Lock()
	cfg, err := v.providerConfigLocked(providerName)
	v.mu.Unlock()
	if err != nil {
		return "", err
	}
	return v.ext.resolve(cfg, Link{Provider: providerName, Ref: ref})
}
