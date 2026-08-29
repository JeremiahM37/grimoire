package identity

import (
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// FromEnv builds the resolver an operator asked for.
//
// GRIMOIRE_IDENTITY is a comma-separated list naming the backends to run, in
// order. Unset means no identity resolution at all, which is the default and
// the behaviour every existing deployment already has: callers keep the
// self-asserted agent name they have always had, and nothing about
// authorization changes.
//
// Naming the backends explicitly, rather than enabling whichever ones happen
// to be configurable, is what keeps this predictable. An operator who sets a
// ZeroTier token to try something should not thereby change who a caller is.
func FromEnv() *Resolver {
	names := strings.TrimSpace(os.Getenv("GRIMOIRE_IDENTITY"))
	if names == "" || strings.EqualFold(names, "off") || names == "0" {
		return New()
	}
	var backends []Backend
	for _, raw := range strings.Split(names, ",") {
		switch strings.ToLower(strings.TrimSpace(raw)) {
		case "":
		case "tailscale", "headscale":
			// Headscale speaks the same protocol and the client is the same
			// tailscaled, so the same LocalAPI answers.
			backends = append(backends, tailscaleFromEnv())
		case "zerotier":
			if z := zerotierFromEnv(); z != nil {
				backends = append(backends, z)
			}
		case "mtls":
			backends = append(backends, &MTLS{Field: os.Getenv("GRIMOIRE_MTLS_FIELD")})
		case "proxy":
			if p := proxyFromEnv(); p != nil {
				backends = append(backends, p)
			}
		}
	}
	return New(backends...)
}

func tailscaleFromEnv() *Tailscale {
	t := &Tailscale{Endpoint: strings.TrimSpace(os.Getenv("GRIMOIRE_TAILSCALE_ENDPOINT"))}
	if r := ParsePrefixes(os.Getenv("GRIMOIRE_TAILSCALE_RANGES")); len(r) > 0 {
		t.Ranges = r
	}
	t.TTL = envDuration("GRIMOIRE_IDENTITY_TTL", 0)
	return t
}

func zerotierFromEnv() *ZeroTier {
	network := strings.TrimSpace(os.Getenv("GRIMOIRE_ZEROTIER_NETWORK"))
	if network == "" {
		// Without a network there is nothing to look identities up in, and a
		// backend that can never answer should not be listed as running.
		return nil
	}
	z := &ZeroTier{
		API:     strings.TrimSpace(os.Getenv("GRIMOIRE_ZEROTIER_API")),
		Network: network,
		Token:   envSecret("GRIMOIRE_ZEROTIER_TOKEN", "GRIMOIRE_ZEROTIER_TOKEN_FILE"),
		Ranges:  ParsePrefixes(os.Getenv("GRIMOIRE_ZEROTIER_RANGES")),
		TTL:     envDuration("GRIMOIRE_IDENTITY_TTL", 0),
	}
	return z
}

func proxyFromEnv() *Proxy {
	from := ParsePrefixes(os.Getenv("GRIMOIRE_IDENTITY_PROXY_FROM"))
	if len(from) == 0 {
		// A proxy backend with nobody trusted would read a header anyone can
		// set. Refusing to run is the only safe response to that configuration.
		return nil
	}
	return &Proxy{
		From:         from,
		Header:       strings.TrimSpace(os.Getenv("GRIMOIRE_IDENTITY_PROXY_HEADER")),
		DeviceHeader: strings.TrimSpace(os.Getenv("GRIMOIRE_IDENTITY_PROXY_DEVICE_HEADER")),
	}
}

// envSecret reads a value directly or from a file, because a token on a
// command line is a token in the process table.
func envSecret(valueVar, fileVar string) string {
	if p := strings.TrimSpace(os.Getenv(fileVar)); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return strings.TrimSpace(os.Getenv(valueVar))
}

func envDuration(name string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil && d >= 0 {
		return d
	}
	return fallback
}

// LoopbackOnly is the prefix set meaning "this machine", used as the default
// trusted proxy set in documentation examples.
var LoopbackOnly = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
}
