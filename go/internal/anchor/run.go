package anchor

import (
	"context"
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
)

// binaryDir is where binary_version looks. A variable so tests can point it at
// a fake binary; production never changes it.
var binaryDir = "/usr/local/bin"

// Run validates an anchor and checks it once. It never runs a shell: every
// command is an argument vector. A remote anchor is not checked from here and
// reports Skipped.
func Run(ctx context.Context, a Anchor) Result {
	if err := Validate(a); err != nil {
		return Result{Status: Error, Message: err.Error()}
	}
	if a.Type == Remote {
		return Result{Status: Skipped, Message: "remote: not checked from this host"}
	}
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	ok, why, err := check(ctx, a)
	switch {
	case err != nil:
		return Result{Status: Error, Message: err.Error()}
	case ok:
		return Result{Status: Pass}
	default:
		return Result{Status: Fail, Message: why}
	}
}

// check runs one validated, non-remote probe. ok is the verdict, why says what
// was missing when it is false, and err means the probe could not answer.
func check(ctx context.Context, a Anchor) (ok bool, why string, err error) {
	arg := func(k string) string { return a.Args[k] }
	switch a.Type {
	case FileExists:
		return statKind(arg("path"), false)
	case DirExists:
		return statKind(arg("path"), true)
	case RefPath:
		return lexists(arg("path"))
	case PathAbsent:
		exists, _, err := lexists(arg("path"))
		if err != nil {
			return false, "", err
		}
		return !exists, "path exists", nil
	case SymlinkTo:
		return symlinkTo(arg("path"), arg("target"))
	case FileContains:
		return fileContains(arg("path"), arg("text"))
	case UnitActive:
		state, err := unitState(ctx, arg("unit"))
		return state == "active", "not active", err
	case UnitInactive:
		state, err := unitState(ctx, arg("unit"))
		return state != "active", "active", err
	case UnitEnabled:
		state, err := unitEnabled(ctx, arg("unit"))
		return state, "not enabled", err
	case RefUnit:
		exists, err := unitExists(ctx, arg("unit"))
		return exists, "unit not found", err
	case PortListening:
		open, err := listening(ctx, arg("port"))
		return open, "not listening", err
	case PortClosed:
		open, err := listening(ctx, arg("port"))
		return !open, "listening", err
	case HTTPStatus:
		want, _ := strconv.Atoi(arg("status"))
		status, _, err := httpGet(ctx, arg("url"))
		return status == want, "status " + strconv.Itoa(status), err
	case HTTPContains:
		status, body, err := httpGet(ctx, arg("url"))
		if err != nil {
			return false, "", err
		}
		if status < 200 || status > 299 {
			return false, "status " + strconv.Itoa(status), nil
		}
		return strings.Contains(string(body), arg("text")), "text not present", nil
	case BinaryVersion:
		return binaryVersion(ctx, arg("bin"), arg("text"))
	case GitCommit:
		return gitTest(ctx, arg("repo"), "cat-file", "-e", arg("ref")+"^{commit}")
	case GitBranch:
		return gitTest(ctx, arg("repo"), "show-ref", "--verify", "--quiet", "refs/heads/"+arg("branch"))
	}
	return false, "", errors.New("unknown anchor type " + a.Type)
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
func fileContains(path, text string) (bool, string, error) {
	clean := filepath.Clean(path)
	if deniedPath(clean) {
		return false, "", errors.New("denied path")
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if errors.Is(err, fs.ErrNotExist) {
		return false, "missing", nil
	}
	if err != nil {
		return false, "", err
	}
	if deniedPath(resolved) {
		return false, "", errors.New("denied path")
	}
	f, err := os.Open(resolved)
	if err != nil {
		return false, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, "", err
	}
	if !fi.Mode().IsRegular() {
		return false, "", errors.New("not a regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFileRead))
	if err != nil {
		return false, "", err
	}
	return strings.Contains(string(data), text), "text not present", nil
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

func unitEnabled(ctx context.Context, unit string) (bool, error) {
	for _, scope := range [][]string{{}, {"--user"}} {
		out, err := systemctl(ctx, append(scope, "is-enabled", unit)...)
		if err != nil {
			return false, err
		}
		switch out {
		case "enabled", "static", "enabled-runtime":
			return true, nil
		}
	}
	return false, nil
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
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		local := f[3]
		if i := strings.LastIndex(local, ":"); i >= 0 && local[i+1:] == port {
			return true, nil
		}
	}
	return false, nil
}

// binaryVersion runs the grimoire or lectern binary's version command. The
// path is fixed by name, so the argument can never choose a program.
func binaryVersion(ctx context.Context, bin, text string) (bool, string, error) {
	out, err := command(ctx, filepath.Join(binaryDir, bin), "version")
	if err != nil {
		return false, "", err
	}
	return strings.Contains(out, text), "version text not present", nil
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
