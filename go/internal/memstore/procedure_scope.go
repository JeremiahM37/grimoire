package memstore

import (
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A procedure can name a path, a unit or a port that belongs to another
// machine: "ssh mediaserver; systemctl restart foo.service", "pct exec 200 --
// ls /opt/docker", "scp build@10.0.0.7:/srv/out ~/". This host cannot answer
// for those, and answering "does not exist" would flag a correct procedure as
// stale. Such references are reported as unverifiable here: they neither pass
// nor fail, and a procedure with only those is counted as seen, not proven.

// Unverifiable detail prefix.
const UnverifiableHere = "unverifiable here"

var (
	remoteExecRE = regexp.MustCompile(`(?i)\b(?:ssh|scp|sftp|rsync|mosh)\b|\bpct\s+(?:exec|push|pull|enter)\b|\bqm\s+(?:guest\s+)?(?:exec|terminal)\b|` +
		`\b(?:docker|podman|kubectl|lxc|incus|nsenter)\s+(?:exec|run\s+--rm)\b|\blxc\s+\d+\b|\bpvesh\b|\bon\s+(?:the\s+)?(?:remote|other)\s+(?:host|machine|box|node|server)\b`)
	userAtHostRE = regexp.MustCompile(`\b[A-Za-z0-9._-]+@([A-Za-z0-9][A-Za-z0-9.-]*)`)
	hostColonRE  = regexp.MustCompile(`(?:^|[\s"'(=])([A-Za-z][A-Za-z0-9.-]*):(?:/|~)`)
	ipv4RE       = regexp.MustCompile(`\b(\d{1,3}(?:\.\d{1,3}){3})\b`)
	fqdnRE       = regexp.MustCompile(`(?i)\b([a-z0-9][a-z0-9-]*(?:\.[a-z0-9-]+)*\.(?:ts\.net|internal|local|lan|home\.arpa|localdomain))\b`)
)

// schemeNames are "word:/" prefixes that are URL schemes, not hosts.
var schemeNames = map[string]bool{"http": true, "https": true, "file": true, "ftp": true, "ssh": true, "git": true, "s3": true, "sftp": true}

// hostScope answers whether a line of a procedure is about this machine.
type hostScope struct {
	names  map[string]bool // this host's names, lowercased
	ips    map[string]bool // this host's addresses
	others []string        // other hosts' names (settings), lowercased
	home   string
}

func newHostScope(env Env) hostScope {
	h := hostScope{names: map[string]bool{"localhost": true}, ips: map[string]bool{}, home: env.Home}
	if env.HostNames != nil {
		for _, n := range env.HostNames {
			h.addName(n)
		}
	} else if n, err := os.Hostname(); err == nil {
		h.addName(n)
	}
	var ips []string
	if env.HostIPs != nil {
		ips = env.HostIPs()
	} else {
		ips = localIPs()
	}
	for _, ip := range ips {
		h.ips[ip] = true
	}
	for _, o := range env.OtherHosts {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" && !h.names[o] {
			h.others = append(h.others, o)
		}
	}
	return h
}

func (h hostScope) addName(n string) {
	n = strings.ToLower(strings.TrimSpace(n))
	if n == "" {
		return
	}
	h.names[n] = true
	if i := strings.IndexByte(n, '.'); i > 0 {
		h.names[n[:i]] = true
	}
}

func localIPs() []string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	var out []string
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func (h hostScope) localName(n string) bool {
	n = strings.ToLower(strings.Trim(n, ".:"))
	if h.names[n] {
		return true
	}
	if i := strings.IndexByte(n, '.'); i > 0 && h.names[n[:i]] {
		return true
	}
	return false
}

// foreignLine returns why the line is about another machine, or "".
func (h hostScope) foreignLine(line string) string {
	if m := remoteExecRE.FindString(line); m != "" {
		return "remote context (" + strings.TrimSpace(m) + ")"
	}
	for _, m := range userAtHostRE.FindAllStringSubmatch(line, -1) {
		if !h.localName(m[1]) && !isLoopback(m[1]) {
			return "other host (" + m[1] + ")"
		}
	}
	for _, m := range hostColonRE.FindAllStringSubmatch(line, -1) {
		// "host:/path" is scp syntax whatever the host is called.
		if !schemeNames[strings.ToLower(m[1])] && !h.localName(m[1]) {
			return "other host (" + m[1] + ")"
		}
	}
	for _, m := range ipv4RE.FindAllStringSubmatch(line, -1) {
		ip := net.ParseIP(m[1])
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || h.ips[ip.String()] {
			continue
		}
		return "other host (" + m[1] + ")"
	}
	for _, m := range fqdnRE.FindAllStringSubmatch(line, -1) {
		if !h.localName(m[1]) {
			return "other host (" + m[1] + ")"
		}
	}
	low := strings.ToLower(line)
	for _, o := range h.others {
		if containsWord(low, o) {
			return "other host (" + o + ")"
		}
	}
	return ""
}

func isLoopback(n string) bool { return n == "localhost" || n == "127.0.0.1" || n == "::1" }

func containsWord(s, w string) bool {
	for i := 0; ; {
		j := strings.Index(s[i:], w)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(w)
		okL := a == 0 || !isWordByte(s[a-1])
		okR := b == len(s) || !isWordByte(s[b])
		if okL && okR {
			return true
		}
		i = a + 1
	}
}

func isWordByte(c byte) bool {
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// lineAt returns the line of text holding byte offset i.
func lineAt(text string, i int) string {
	if i < 0 || i > len(text) {
		return ""
	}
	a := strings.LastIndexByte(text[:i], '\n') + 1
	b := strings.IndexByte(text[i:], '\n')
	if b < 0 {
		return text[a:]
	}
	return text[a : i+b]
}

// foreignPath reports a path that is not this machine's: another user's home,
// or /root when this is not root.
func (h hostScope) foreignPath(p string) string {
	if strings.HasPrefix(p, "/home/") {
		rest := strings.TrimPrefix(p, "/home/")
		user, _, _ := strings.Cut(rest, "/")
		if h.home != "" && filepath.Dir(h.home) == "/home" && filepath.Base(h.home) != user {
			return "another user's home (" + user + ")"
		}
	}
	if strings.HasPrefix(p, "/root/") && h.home != "/root" && h.home != "" {
		return "root's home on a host this user cannot read"
	}
	return ""
}
