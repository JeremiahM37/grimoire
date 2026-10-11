// Package anchor is the typed, read-only check a stored memory fact can carry.
//
// A fact that says how something IS now ("the router is at 192.168.0.1") can
// go false without anyone writing to the memory. An anchor names the thing the
// fact is about (a file and a literal in it, a unit, a port, a commit) so the
// store can ask the system whether the claim still holds. Probes are fixed and
// read-only: there is no shell, no arbitrary command and no write.
//
// The text form is one token per fact's list, safe inside a trailer:
//
//	type;key=value;key=value|type;key=value
//
// Keys and values are percent-escaped, so nothing in the encoding is a space,
// a '>', a '|' or a ';' except as a separator. Arguments are written in sorted
// key order, which makes the encoding deterministic.
package anchor

import (
	"fmt"
	"net/netip"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Anchor is one typed probe with its arguments.
type Anchor struct {
	Type string
	Args map[string]string
}

// The probe types a stored claim may carry (see docs/MEMORY-ANCHORS.md).
const (
	FileExists    = "file_exists"
	DirExists     = "dir_exists"
	PathAbsent    = "path_absent"
	SymlinkTo     = "symlink_to"
	FileContains  = "file_contains"
	UnitActive    = "unit_active"
	UnitInactive  = "unit_inactive"
	UnitEnabled   = "unit_enabled"
	PortListening = "port_listening"
	PortClosed    = "port_closed"
	HTTPStatus    = "http_status"
	HTTPContains  = "http_contains"
	BinaryVersion = "binary_version"
	GitCommit     = "git_commit"
	GitBranch     = "git_branch"
	Remote        = "remote"
)

// Reference-only types. The dead-reference scan (ExtractRefs) produces them to
// test whether a note's path or unit still exists. They are checkable like the
// probes but are not probes: Storable refuses them, so a fact can never carry
// one. Their existence check is weaker than a claim, which is the point.
const (
	RefPath = "ref_path"
	RefUnit = "ref_unit"
)

// spec is what a type takes: every argument it requires and nothing else.
type spec struct {
	required []string
}

var specs = map[string]spec{
	FileExists:    {[]string{"path"}},
	DirExists:     {[]string{"path"}},
	PathAbsent:    {[]string{"path"}},
	SymlinkTo:     {[]string{"path", "target"}},
	FileContains:  {[]string{"path", "text"}},
	UnitActive:    {[]string{"unit"}},
	UnitInactive:  {[]string{"unit"}},
	UnitEnabled:   {[]string{"unit"}},
	PortListening: {[]string{"port"}},
	PortClosed:    {[]string{"port"}},
	HTTPStatus:    {[]string{"url", "status"}},
	HTTPContains:  {[]string{"url", "text"}},
	BinaryVersion: {[]string{"bin", "text"}},
	GitCommit:     {[]string{"repo", "ref"}},
	GitBranch:     {[]string{"repo", "branch"}},
	Remote:        {[]string{"what"}},
	RefPath:       {[]string{"path"}},
	RefUnit:       {[]string{"unit"}},
}

// Storable reports whether a type may be stored on a fact: every probe type,
// and not the reference-only ones.
func Storable(typ string) bool {
	return typ != RefPath && typ != RefUnit && specs[typ].required != nil
}

// Validate reports whether an anchor is well formed. It checks the type, the
// exact argument set, that paths are absolute, and that each value is in the
// form its probe needs. It never touches the system.
func Validate(a Anchor) error {
	sp, ok := specs[a.Type]
	if !ok {
		return fmt.Errorf("unknown anchor type %q", a.Type)
	}
	for k := range a.Args {
		if !contains(sp.required, k) {
			return fmt.Errorf("%s does not take argument %q", a.Type, k)
		}
	}
	for _, k := range sp.required {
		v, ok := a.Args[k]
		if !ok || v == "" {
			return fmt.Errorf("%s needs %s", a.Type, k)
		}
	}
	for k, v := range a.Args {
		if strings.ContainsFunc(v, isControl) {
			return fmt.Errorf("%s: %s contains a control character", a.Type, k)
		}
	}
	for _, k := range []string{"path", "target", "repo"} {
		if v, ok := a.Args[k]; ok && !filepath.IsAbs(v) {
			return fmt.Errorf("%s: %s must be an absolute path", a.Type, k)
		}
	}
	if u, ok := a.Args["unit"]; ok && !validUnit(u) {
		return fmt.Errorf("invalid unit name %q", u)
	}
	if p, ok := a.Args["port"]; ok {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 || strings.TrimLeft(p, "0123456789") != "" {
			return fmt.Errorf("port must be 1 to 65535, not %q", p)
		}
	}
	if s, ok := a.Args["status"]; ok {
		if n, err := strconv.Atoi(s); err != nil || n < 100 || n > 599 || strings.TrimLeft(s, "0123456789") != "" {
			return fmt.Errorf("status must be an HTTP status code, not %q", s)
		}
	}
	if u, ok := a.Args["url"]; ok {
		if err := validURL(u); err != nil {
			return err
		}
	}
	if b, ok := a.Args["bin"]; ok && b != "grimoire" && b != "lectern" {
		return fmt.Errorf("binary_version only checks grimoire or lectern, not %q", b)
	}
	if r, ok := a.Args["ref"]; ok && !validRef(r) {
		return fmt.Errorf("invalid git ref %q", r)
	}
	if b, ok := a.Args["branch"]; ok && !validRef(b) {
		return fmt.Errorf("invalid branch name %q", b)
	}
	return nil
}

// FromMap reads an anchor from the JSON object a caller sent, such as
// {"type":"file_contains","path":"/x","text":"y"}. Numbers are accepted for
// port and status; anything else is an error rather than a silent guess.
func FromMap(m map[string]any) (Anchor, error) {
	typ, ok := m["type"].(string)
	if !ok {
		return Anchor{}, fmt.Errorf("an anchor needs a string type")
	}
	a := Anchor{Type: typ, Args: map[string]string{}}
	for k, v := range m {
		if k == "type" {
			continue
		}
		switch x := v.(type) {
		case string:
			a.Args[k] = x
		case float64:
			a.Args[k] = strconv.FormatFloat(x, 'f', -1, 64)
		default:
			return Anchor{}, fmt.Errorf("anchor argument %q must be a string or a number", k)
		}
	}
	return a, nil
}

// Encode renders a list of anchors as the single token stored in a trailer.
// Encode(nil) is "".
func Encode(as []Anchor) string {
	parts := make([]string, 0, len(as))
	for _, a := range as {
		var b strings.Builder
		b.WriteString(escape(a.Type))
		keys := make([]string, 0, len(a.Args))
		for k := range a.Args {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteByte(';')
			b.WriteString(escape(k))
			b.WriteByte('=')
			b.WriteString(escape(a.Args[k]))
		}
		parts = append(parts, b.String())
	}
	return strings.Join(parts, "|")
}

// Decode reads what Encode wrote. Decode("") is nil, with no error.
func Decode(s string) ([]Anchor, error) {
	if s == "" {
		return nil, nil
	}
	var out []Anchor
	for _, seg := range strings.Split(s, "|") {
		fields := strings.Split(seg, ";")
		typ, err := unescape(fields[0])
		if err != nil {
			return nil, err
		}
		if typ == "" {
			return nil, fmt.Errorf("anchor with no type in %q", s)
		}
		a := Anchor{Type: typ, Args: map[string]string{}}
		for _, f := range fields[1:] {
			rawKey, rawVal, ok := strings.Cut(f, "=")
			if !ok {
				return nil, fmt.Errorf("anchor argument without '=' in %q", s)
			}
			key, err := unescape(rawKey)
			if err != nil {
				return nil, err
			}
			if key == "" {
				return nil, fmt.Errorf("anchor argument with no name in %q", s)
			}
			val, err := unescape(rawVal)
			if err != nil {
				return nil, err
			}
			if _, dup := a.Args[key]; dup {
				return nil, fmt.Errorf("anchor argument %q given twice", key)
			}
			a.Args[key] = val
		}
		out = append(out, a)
	}
	return out, nil
}

// escapeSafe reports whether a byte may appear unescaped in the encoding.
// '>', '|', ';', '=', '%', spaces and everything non-ASCII are escaped, which
// is what keeps "-->" and field separators out of a trailer.
func escapeSafe(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	}
	return strings.IndexByte("-._~/:@+,", c) >= 0
}

func escape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escapeSafe(c) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func unescape(s string) (string, error) {
	if !strings.Contains(s, "%") {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(s) {
			return "", fmt.Errorf("truncated escape in anchor %q", s)
		}
		n, err := strconv.ParseUint(s[i+1:i+3], 16, 8)
		if err != nil {
			return "", fmt.Errorf("bad escape in anchor %q", s)
		}
		b.WriteByte(byte(n))
		i += 2
	}
	return b.String(), nil
}

var unitRE = regexp.MustCompile(`^[\w@.\-:]+$`)

// validUnit accepts a systemd unit name. A leading dash is refused as well as
// the character set, because the name is passed to systemctl as an argument.
func validUnit(u string) bool {
	return unitRE.MatchString(u) && !strings.HasPrefix(u, "-")
}

var refRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// validRef accepts a git ref or branch name, again because it becomes an
// argument. ".." and a leading dash are refused.
func validRef(r string) bool {
	return refRE.MatchString(r) && !strings.Contains(r, "..")
}

var tailnet = netip.MustParsePrefix("100.64.0.0/10")

// validURL accepts http and https URLs whose host is a literal address on this
// machine or on the tailnet. Names are refused: resolving one would be a
// lookup the store did not ask for.
func validURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("url must be http or https: %q", raw)
	}
	addr, err := netip.ParseAddr(u.Hostname())
	if err != nil {
		return fmt.Errorf("url host must be a literal 127.x, ::1 or 100.64/10 address, not %q", u.Hostname())
	}
	if !addr.IsLoopback() && !tailnet.Contains(addr) {
		return fmt.Errorf("url host %s is neither loopback nor tailnet", addr)
	}
	return nil
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f }

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
