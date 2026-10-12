package anchor

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"sync"
)

// Other machines. A fact can be about a machine that is not this one, and its
// probes must run there. The allowlist is the only way to reach another host:
// GRIMOIRE_ANCHOR_HOSTS names each host and the ssh target that reaches it, and
// nothing else is ever contacted. Empty by default, so nothing leaves this
// machine unless the operator asked for it.

// hostNameRE is the one shape a host label may take. It is the same shape the
// write API accepts, and it can never start an option or a command.
var hostNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.\-]{0,62}$`)

func validHost(h string) bool { return hostNameRE.MatchString(h) }

var (
	hostsMu sync.RWMutex
	hostMap = map[string]string{}
)

// localHostFn is this machine's short name. A variable so tests can be a
// machine with another name.
var localHostFn = shortHostname

// LocalHost is this machine's short, lowercased name.
func LocalHost() string { return strings.ToLower(localHostFn()) }

func shortHostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	if i := strings.IndexByte(h, '.'); i >= 0 {
		h = h[:i]
	}
	return h
}

// SetHosts replaces the allowlist. Names are lowercased. The server calls it
// once at start, from GRIMOIRE_ANCHOR_HOSTS.
func SetHosts(m map[string]string) {
	next := make(map[string]string, len(m))
	for name, target := range m {
		next[strings.ToLower(name)] = target
	}
	hostsMu.Lock()
	hostMap = next
	hostsMu.Unlock()
}

// hostTarget is the ssh target for a host name, if the allowlist has one.
func hostTarget(name string) (string, bool) {
	hostsMu.RLock()
	defer hostsMu.RUnlock()
	t, ok := hostMap[strings.ToLower(name)]
	return t, ok
}

// ParseHosts reads GRIMOIRE_ANCHOR_HOSTS: comma-separated name=sshtarget pairs,
// for example "mediaserver=root@100.64.0.10,docker-server=root@100.64.0.11".
// A target may not start with '-' or contain whitespace, because it is passed to
// ssh as an argument.
func ParseHosts(raw string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, target, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("GRIMOIRE_ANCHOR_HOSTS wants name=sshtarget, not %q", part)
		}
		name = strings.ToLower(strings.TrimSpace(name))
		target = strings.TrimSpace(target)
		if !validHost(name) {
			return nil, fmt.Errorf("GRIMOIRE_ANCHOR_HOSTS: %q is not a host name", name)
		}
		if target == "" || strings.HasPrefix(target, "-") || strings.ContainsFunc(target, isSpaceOrControl) {
			return nil, fmt.Errorf("GRIMOIRE_ANCHOR_HOSTS: the ssh target for %s is not usable", name)
		}
		out[name] = target
	}
	return out, nil
}

func isSpaceOrControl(r rune) bool { return r <= 0x20 || r == 0x7f }

// Inherit gives every anchor that names no host the fact's host, when the fact
// is about another machine. A claim written about mediaserver is checked on
// mediaserver even when the writer did not repeat that. Anchors on this machine
// are returned as they are. It does not run anything; callers apply it where the
// anchors of a fact are run, not inside Run.
func Inherit(as []Anchor, factHost string) []Anchor {
	factHost = strings.ToLower(factHost)
	if factHost == "" || factHost == LocalHost() {
		return as
	}
	out := make([]Anchor, len(as))
	for i, a := range as {
		if _, has := a.Args[argHost]; has || a.Type == Remote {
			out[i] = a
			continue
		}
		args := make(map[string]string, len(a.Args)+1)
		for k, v := range a.Args {
			args[k] = v
		}
		args[argHost] = factHost
		out[i] = Anchor{Type: a.Type, Args: args}
	}
	return out
}
