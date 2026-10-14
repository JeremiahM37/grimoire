package anchor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Status is the outcome of one probe.
type Status string

const (
	Pass    Status = "pass"
	Fail    Status = "fail"
	Error   Status = "error"
	Skipped Status = "skipped"
)

// Result is what running one anchor found. Message is short and never holds
// fact text; for Error it says why the probe could not answer.
type Result struct {
	Status  Status
	Message string
	// Observed is a short sha256 hex (16 bytes) of what the probe saw, where
	// the probe has one: the bytes of a file, a unit's state, a body, a version
	// string, a git revision. It is never the content itself. Empty when the
	// probe has nothing to observe, or when observing it is refused (a denied
	// path is never hashed: a digest of a secret is a guessing oracle).
	Observed string
}

const (
	// probeTimeout bounds every external command.
	probeTimeout = 10 * time.Second
	// httpTimeout bounds one GET.
	httpTimeout = 8 * time.Second
	// maxHTTPBody is how much of a response an http probe reads.
	maxHTTPBody = 200 << 10
	// maxFileRead is how much of a file file_contains reads.
	maxFileRead = 5 << 20
	// maxObserveRead is how much of a file is hashed for Observed.
	maxObserveRead = 1 << 20
)

// binaryDir is where binary_version looks. A variable so tests can point it at
// a fake binary; production never changes it.
var binaryDir = "/usr/local/bin"

// Skip messages. Reports count skipped anchors by these, so they are constants
// and not typed inline.
const (
	SkipRemote         = "remote: not checked from this host"
	SkipHostNotAllowed = "host not in GRIMOIRE_ANCHOR_HOSTS"
	SkipUnsafeArg      = "argument not safe for a remote probe"
	SkipBinaryRemote   = "binary_version only checks this host"
)

// SkipReason names why a Skipped result was skipped, for the admin report.
// Anything else is "other".
func SkipReason(res Result) string {
	switch res.Message {
	case SkipRemote:
		return "remote"
	case SkipHostNotAllowed:
		return "host_not_allowlisted"
	case SkipUnsafeArg:
		return "unsafe_argument"
	}
	return "other"
}

// Run validates an anchor and checks it once. It never runs a shell: every
// command is an argument vector. A remote anchor is not checked from here and
// reports Skipped. An anchor naming another host runs over ssh if that host is
// in the allowlist (see hosts.go and remote.go).
func Run(ctx context.Context, a Anchor) Result {
	if err := Validate(a); err != nil {
		return Result{Status: Error, Message: err.Error()}
	}
	if a.Type == Remote {
		return Result{Status: Skipped, Message: SkipRemote}
	}
	host := strings.ToLower(a.Args[argHost])
	if host == "" || host == LocalHost() || isHTTP(a.Type) {
		// http probes always run from here: the address they name is the one
		// this machine must reach, whichever host the fact was about.
		return runLocal(ctx, a)
	}
	return runRemote(ctx, a, host)
}

// runLocal checks a validated anchor on this machine.
func runLocal(ctx context.Context, a Anchor) Result {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ok, why, obs, err := check(ctx, a)
	switch {
	case err != nil:
		return Result{Status: Error, Message: err.Error()}
	case ok:
		return Result{Status: Pass, Observed: obs}
	default:
		return Result{Status: Fail, Message: why, Observed: obs}
	}
}

func isHTTP(typ string) bool { return typ == HTTPStatus || typ == HTTPContains }

// check runs one validated, non-remote probe. ok is the verdict, why says what
// was missing when it is false, obs is the observed value (see Result.Observed),
// and err means the probe could not answer.
func check(ctx context.Context, a Anchor) (ok bool, why, obs string, err error) {
	arg := func(k string) string { return a.Args[k] }
	switch a.Type {
	case FileExists:
		ok, why, err = statKind(arg("path"), false)
		if ok {
			obs = observeFile(arg("path"))
		}
		return ok, why, obs, err
	case DirExists:
		ok, why, err = statKind(arg("path"), true)
		return ok, why, "", err
	case RefPath:
		ok, why, err = lexists(arg("path"))
		return ok, why, "", err
	case PathAbsent:
		exists, _, err := lexists(arg("path"))
		if err != nil {
			return false, "", "", err
		}
		return !exists, "path exists", "", nil
	case SymlinkTo:
		ok, why, err = symlinkTo(arg("path"), arg("target"))
		return ok, why, "", err
	case FileContains:
		return fileContains(arg("path"), arg("text"))
	case UnitActive:
		state, err := unitState(ctx, arg("unit"))
		return state == "active", "not active", digestIf(err, state), err
	case UnitInactive:
		state, err := unitState(ctx, arg("unit"))
		return state != "active", "active", digestIf(err, state), err
	case UnitEnabled:
		enabled, state, err := unitEnabled(ctx, arg("unit"))
		return enabled, "not enabled", digestIf(err, state), err
	case RefUnit:
		exists, err := unitExists(ctx, arg("unit"))
		return exists, "unit not found", "", err
	case PortListening:
		open, err := listening(ctx, arg("port"))
		return open, "not listening", digestIf(err, portState(arg("port"), open)), err
	case PortClosed:
		open, err := listening(ctx, arg("port"))
		return !open, "listening", digestIf(err, portState(arg("port"), open)), err
	case HTTPStatus:
		want, _ := strconv.Atoi(arg("status"))
		status, body, err := httpGet(ctx, arg("url"))
		return status == want, "status " + strconv.Itoa(status), digestIf(err, string(body)), err
	case HTTPContains:
		status, body, err := httpGet(ctx, arg("url"))
		if err != nil {
			return false, "", "", err
		}
		if status < 200 || status > 299 {
			return false, "status " + strconv.Itoa(status), "", nil
		}
		return strings.Contains(string(body), arg("text")), "text not present", digest(body), nil
	case BinaryVersion:
		return binaryVersion(ctx, arg("bin"), arg("text"))
	case GitCommit:
		ok, why, err := gitTest(ctx, arg("repo"), "cat-file", "-e", arg("ref")+"^{commit}")
		if !ok || err != nil {
			return ok, why, "", err
		}
		return ok, why, gitRev(ctx, arg("repo"), arg("ref")+"^{commit}"), nil
	case GitBranch:
		ok, why, err := gitTest(ctx, arg("repo"), "show-ref", "--verify", "--quiet", "refs/heads/"+arg("branch"))
		if !ok || err != nil {
			return ok, why, "", err
		}
		return ok, why, gitRev(ctx, arg("repo"), "refs/heads/"+arg("branch")), nil
	}
	return false, "", "", errors.New("unknown anchor type " + a.Type)
}

// digest is the observed-value hash: the first 16 bytes of sha256, in hex.
func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:16])
}

// digestIf hashes an observed string, or returns "" when the probe failed and
// so observed nothing.
func digestIf(err error, s string) string {
	if err != nil {
		return ""
	}
	return digest([]byte(s))
}

// portState is the observed value of a port probe. A port carries membership
// only: whether something is bound to it, not which process.
func portState(port string, open bool) string {
	if open {
		return "listening:" + port
	}
	return "closed:" + port
}

// observeFile hashes the first maxObserveRead bytes of a regular file for
// file_exists. Denied paths (and links into them) are never read for this.
func observeFile(path string) string {
	clean := filepath.Clean(path)
	if deniedPath(clean) {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil || deniedPath(resolved) {
		return ""
	}
	f, err := os.Open(resolved)
	if err != nil {
		return ""
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxObserveRead))
	if err != nil {
		return ""
	}
	return digest(data)
}

// gitRev reads the object a revision names, for the observed value of a git
// probe. An unreadable revision observes nothing.
func gitRev(ctx context.Context, repo, rev string) string {
	out, err := command(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", rev)
	if err != nil || out == "" {
		return ""
	}
	return digest([]byte(out))
}

// statKind answers file_exists (wantDir false) and dir_exists (true). Missing
// is a verdict, not an error: the claim is simply no longer true.
func statKind(path string, wantDir bool) (bool, string, error) {
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if wantDir {
		return fi.IsDir(), "not a directory", nil
	}
	return fi.Mode().IsRegular(), "not a regular file", nil
}

// lexists is true for anything at the path, including a dangling symlink.
func lexists(path string) (bool, string, error) {
	_, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "missing", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, "", nil
}

func symlinkTo(path, target string) (bool, string, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if fi.Mode()&fs.ModeSymlink == 0 {
		return false, "not a symlink", nil
	}
	got, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false, "link does not resolve", nil
	}
	want, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false, "target does not resolve", nil
	}
	return got == want, "points elsewhere", nil
}

// deniedPath reports whether file_contains must refuse a path: secrets, keys,
// the sealing key store, shadow, environment files, and the vault's own
// index directory. The check is textual on purpose; it errs toward refusing.
func deniedPath(p string) bool {
	for _, s := range []string{"/.ai-secrets", "/.ssh/", "seal-keys", "/etc/shadow"} {
		if strings.Contains(p, s) {
			return true
		}
	}
	if strings.HasSuffix(filepath.Base(p), ".env") {
		return true
	}
	if vault := os.Getenv("GRIMOIRE_VAULT"); vault != "" {
		dirs := []string{filepath.Join(vault, ".grimoire")}
		if rv, err := filepath.EvalSymlinks(vault); err == nil {
			dirs = append(dirs, filepath.Join(rv, ".grimoire"))
		}
		for _, d := range dirs {
			if p == d || strings.HasPrefix(p, d+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// fileContains reads at most maxFileRead bytes of a regular file and looks for
// a literal. The denylist is applied to the path as written and again after
// symlinks are resolved, so a link into a secret directory is refused too.
func fileContains(path, text string) (bool, string, string, error) {
	clean := filepath.Clean(path)
	if deniedPath(clean) {
		return false, "", "", errors.New("denied path")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "missing", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	if deniedPath(resolved) {
		return false, "", "", errors.New("denied path")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return false, "", "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, "", "", err
	}
	if !fi.Mode().IsRegular() {
		return false, "", "", errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileRead))
	if err != nil {
		return false, "", "", err
	}
	hashed := data
	if len(hashed) > maxObserveRead {
		hashed = hashed[:maxObserveRead]
	}
	return strings.Contains(string(data), text), "text not present", digest(hashed), nil
}

// unitState asks the system manager, then the user manager, for the unit's
// active state. Either one reporting "active" makes the unit active.
func unitState(ctx context.Context, unit string) (string, error) {
	for _, scope := range [][]string{{}, {"--user"}} {
		out, err := systemctl(ctx, append(scope, "is-active", unit)...)
		if err != nil {
			return "", err
		}
		if out == "active" {
			return out, nil
		}
	}
	return "inactive", nil
}

// unitEnabled reports whether the unit is enabled in either manager. The state
// string is what each manager printed, for the observed value.
func unitEnabled(ctx context.Context, unit string) (bool, string, error) {
	var seen []string
	for _, scope := range [][]string{{}, {"--user"}} {
		out, err := systemctl(ctx, append(scope, "is-enabled", unit)...)
		if err != nil {
			return false, "", err
		}
		seen = append(seen, out)
		switch out {
		case "enabled", "static", "enabled-runtime":
			return true, strings.Join(seen, "|"), nil
		}
	}
	return false, strings.Join(seen, "|"), nil
}

// unitExists is true when either manager has a unit file for the name. systemd
// reports "not-found" for a unit it has never heard of.
func unitExists(ctx context.Context, unit string) (bool, error) {
	for _, scope := range [][]string{{}, {"--user"}} {
		out, err := systemctl(ctx, append(scope, "show", "-p", "LoadState", "--value", unit)...)
		if err != nil {
			return false, err
		}
		if out != "" && out != "not-found" {
			return true, nil
		}
	}
	return false, nil
}

// systemctl runs one query. Several queries exit non-zero for an ordinary
// answer ("inactive"), so only a failure to run at all, or a timeout, is an
// error; the printed state is the answer either way.
func systemctl(ctx context.Context, args ...string) (string, error) {
	return command(ctx, "systemctl", args...)
}

// command runs a fixed binary with an argument vector and returns its stdout.
func command(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// listening reports whether anything is bound to the TCP port, read from
// `ss -ltnH`, whose fourth column is the local address.
func listening(ctx context.Context, port string) (bool, error) {
	out, err := command(ctx, "ss", "-ltnH")
	if err != nil {
		return false, err
	}
	return listeningIn(out, port), nil
}

// listeningIn reads `ss -ltnH` output and says whether a socket is bound to the
// port. It is shared by the local and the remote probe.
func listeningIn(out, port string) bool {
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		if i := strings.LastIndex(local, ":"); i >= 0 && local[i+1:] == port {
			return true
		}
	}
	return false
}

// binaryVersion runs the grimoire or lectern binary's version command. The
// path is fixed by name, so the argument can never choose a program.
func binaryVersion(ctx context.Context, bin, text string) (bool, string, string, error) {
	out, err := command(ctx, filepath.Join(binaryDir, bin), "version")
	if err != nil {
		return false, "", "", err
	}
	return strings.Contains(out, text), "version text not present", digest([]byte(out)), nil
}

// gitTest runs git against a repository and reads the exit status: zero is a
// verdict, non-zero is a verdict too (the ref or branch is not there).
func gitTest(ctx context.Context, repo string, args ...string) (bool, string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	err := cmd.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, "", ctxErr
	}
	var ee *exec.ExitError
	if err != nil && !errors.As(err, &ee) {
		return false, "", err
	}
	return err == nil, "not in repository", nil
}

// httpGet is one GET to a literal loopback or tailnet address. Redirects are
// not followed and no proxy is used: the probe must reach the address it names.
func httpGet(ctx context.Context, rawurl string) (int, []byte, error) {
	client := &http.Client{
		Timeout:       httpTimeout,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawurl, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, body, nil
}
