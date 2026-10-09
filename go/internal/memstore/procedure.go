package memstore

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Light verification of procedures. A procedure is checked rarely and
// cheaply: the first check is due at once, then 14 days after each pass,
// doubling up to 120 days, and back to 14 on a fail. The checks are
// deterministic and side-effect free; a failure marks the item "verify" on
// recall and becomes a dream finding. Nothing is ever deleted.
const (
	FirstEveryDays = 14
	MaxEveryDays   = 120
)

// ProcState is the verification state of one procedure.
type ProcState struct {
	VerifiedAt time.Time `json:"verified_at"`
	EveryDays  int       `json:"verify_every"`
	Failed     bool      `json:"failed,omitempty"`
	Detail     string    `json:"detail,omitempty"`
}

// ProcStates is the sidecar file, keyed "note:<file>" or "fact:<id>".
type ProcStates map[string]ProcState

// LoadStates reads the sidecar; a missing or broken file is empty state.
func LoadStates(path string) ProcStates {
	st := ProcStates{}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	if st == nil {
		st = ProcStates{}
	}
	return st
}

// Save writes the sidecar atomically.
func (s ProcStates) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Due reports whether a procedure should be checked now. A failed one is
// checked on every run until it passes; a never-checked one is due at once.
func (p ProcState) Due(now time.Time) bool {
	if p.VerifiedAt.IsZero() || p.Failed {
		return true
	}
	every := p.EveryDays
	if every <= 0 {
		every = FirstEveryDays
	}
	return !now.Before(p.VerifiedAt.AddDate(0, 0, every))
}

// Record applies one check outcome and returns the new state.
func (p ProcState) Record(ok bool, detail string, now time.Time) ProcState {
	if !ok {
		return ProcState{VerifiedAt: now, EveryDays: FirstEveryDays, Failed: true, Detail: detail}
	}
	// EveryDays is the wait until the next check: 14 for a first pass or a
	// pass after a failure, then doubling to the cap.
	every := FirstEveryDays
	if !p.VerifiedAt.IsZero() && !p.Failed && p.EveryDays > 0 {
		every = min(p.EveryDays*2, MaxEveryDays)
	}
	return ProcState{VerifiedAt: now, EveryDays: every}
}

// InitialState reads a note's own verified_at / verify_every, so state kept in
// frontmatter is honoured until the sidecar has an opinion.
func InitialState(fields map[string]string) ProcState {
	var p ProcState
	for _, k := range []string{"metadata.verified_at", "verified_at"} {
		if v := fields[k]; v != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02"} {
				if t, err := time.Parse(layout, v); err == nil {
					p.VerifiedAt = t
					break
				}
			}
			break
		}
	}
	for _, k := range []string{"metadata.verify_every", "verify_every"} {
		if v := strings.TrimSuffix(strings.TrimSpace(fields[k]), "d"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				p.EveryDays = min(n, MaxEveryDays)
				break
			}
		}
	}
	return p
}

// CheckResult is one checker's verdict on one reference found in a procedure.
type CheckResult struct {
	Checker string
	Subject string
	OK      bool
	Detail  string
}

// Checker examines a procedure's text. It must be read-only.
type Checker interface {
	Name() string
	Check(text string) []CheckResult
}

// Env is what checkers may consult. Fields are functions so tests (and
// locked-down hosts) can replace them.
type Env struct {
	Home         string
	Exists       func(path string) bool
	SystemctlCat func(unit string) (found bool, available bool)
	Listening    func(port int) bool
	// Ports is the allowlist of ports a procedure may name and be checked on.
	Ports []int
}

// DefaultEnv checks the real machine.
func DefaultEnv(ports []int) Env {
	home, _ := os.UserHomeDir()
	return Env{
		Home:   home,
		Ports:  ports,
		Exists: func(p string) bool { _, err := os.Stat(p); return err == nil },
		SystemctlCat: func(unit string) (bool, bool) {
			bin, err := exec.LookPath("systemctl")
			if err != nil {
				return false, false
			}
			return exec.Command(bin, "cat", unit).Run() == nil, true
		},
		Listening: func(port int) bool {
			c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 300*time.Millisecond)
			if err != nil {
				return false
			}
			_ = c.Close()
			return true
		},
	}
}

// Checkers returns the built-in checkers over env.
func Checkers(env Env) []Checker {
	return []Checker{pathChecker{env}, unitChecker{env}, portChecker{env}}
}

var (
	pathRE = regexp.MustCompile("(?:^|[\\s`'\"(=:])((?:~|/)[A-Za-z0-9_@%+=.,-]+(?:/[A-Za-z0-9_@%+=.,-]+)+/?)")
	unitRE = regexp.MustCompile(`(?:^|[\s` + "`" + `'"(])([A-Za-z0-9_@:.\\-]+\.(?:service|timer|socket|mount))\b`)
	portRE = regexp.MustCompile(`(?i)(?:\bports?\s*[:=]?\s*|localhost:|127\.0\.0\.1:|0\.0\.0\.0:|\s:)(\d{2,5})\b`)
)

// volatilePrefixes are paths that are not expected to persist.
var volatilePrefixes = []string{"/tmp/", "/proc/", "/sys/", "/dev/", "/run/user/", "/var/tmp/"}

type pathChecker struct{ env Env }

func (pathChecker) Name() string { return "path" }

func (c pathChecker) Check(text string) []CheckResult {
	seen := map[string]bool{}
	var out []CheckResult
	for _, m := range pathRE.FindAllStringSubmatch(text, -1) {
		p := strings.TrimRight(m[1], ".,:;)")
		if seen[p] || strings.ContainsAny(p, "*<>$") {
			continue
		}
		seen[p] = true
		skip := false
		for _, v := range volatilePrefixes {
			if strings.HasPrefix(p, v) {
				skip = true
			}
		}
		// A bare two-segment absolute path ("/api/memory") is far more often
		// a URL route than a file; only paths under a real top directory count.
		if skip || !plausibleRoot(p, c.env.Home) {
			continue
		}
		full := p
		if strings.HasPrefix(p, "~/") {
			full = filepath.Join(c.env.Home, p[2:])
		}
		ok := c.env.Exists(full)
		d := ""
		if !ok {
			d = "path does not exist: " + p
		}
		out = append(out, CheckResult{"path", p, ok, d})
	}
	return out
}

func plausibleRoot(p, home string) bool {
	if strings.HasPrefix(p, "~/") {
		return true
	}
	for _, root := range []string{"/home/", "/etc/", "/opt/", "/usr/", "/var/", "/srv/", "/mnt/", "/root/", "/lib/", "/boot/"} {
		if strings.HasPrefix(p, root) {
			return true
		}
	}
	return home != "" && strings.HasPrefix(p, home+"/")
}

type unitChecker struct{ env Env }

func (unitChecker) Name() string { return "unit" }

func (c unitChecker) Check(text string) []CheckResult {
	if c.env.SystemctlCat == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []CheckResult
	for _, m := range unitRE.FindAllStringSubmatch(text, -1) {
		u := m[1]
		if seen[u] || strings.Contains(u, "..") {
			continue
		}
		seen[u] = true
		found, avail := c.env.SystemctlCat(u)
		if !avail {
			return nil // no systemd here: the check does not apply
		}
		d := ""
		if !found {
			d = "systemd unit not found: " + u
		}
		out = append(out, CheckResult{"unit", u, found, d})
	}
	return out
}

type portChecker struct{ env Env }

func (portChecker) Name() string { return "port" }

func (c portChecker) Check(text string) []CheckResult {
	if len(c.env.Ports) == 0 || c.env.Listening == nil {
		return nil
	}
	allow := map[int]bool{}
	for _, p := range c.env.Ports {
		allow[p] = true
	}
	seen := map[int]bool{}
	var out []CheckResult
	for _, m := range portRE.FindAllStringSubmatch(text, -1) {
		n, _ := strconv.Atoi(m[1])
		if seen[n] || !allow[n] {
			continue
		}
		seen[n] = true
		ok := c.env.Listening(n)
		d := ""
		if !ok {
			d = fmt.Sprintf("nothing is listening on port %d", n)
		}
		out = append(out, CheckResult{"port", strconv.Itoa(n), ok, d})
	}
	return out
}

// RunChecks runs every checker over text and returns the failures.
func RunChecks(text string, checkers []Checker) (all, failed []CheckResult) {
	for _, c := range checkers {
		for _, r := range c.Check(text) {
			all = append(all, r)
			if !r.OK {
				failed = append(failed, r)
			}
		}
	}
	return
}

// FailureDetail renders failures as one short sentence for a finding or a
// verify tag.
func FailureDetail(failed []CheckResult) string {
	parts := make([]string, 0, len(failed))
	for _, f := range failed {
		parts = append(parts, f.Detail)
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}
