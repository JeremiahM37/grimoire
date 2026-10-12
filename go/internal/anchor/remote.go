package anchor

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Probes on another host. The command for a remote probe is built here, from
// fixed text and arguments that match a strict pattern, and runs over ssh as one
// argument. Nothing a writer supplies is ever interpreted by a shell: every
// value is checked against safeArgRE first, and any value that does not pass
// makes the probe Skipped rather than shell-quoted.

// sshBinary and remoteTimeout are variables so a test can point ssh at a fake.
var (
	sshBinary     = "ssh"
	remoteTimeout = 15 * time.Second
)

// safeArgRE is the only shape an argument used on another host may take. There
// is no space, quote, '$', backtick, ';', '~' or glob in it.
var safeArgRE = regexp.MustCompile(`^[A-Za-z0-9_./@+:=,\-]+$`)

var (
	errUnsafeArg  = errors.New(SkipUnsafeArg)
	errDeniedPath = errors.New("denied path")
	errNotRemote  = errors.New("not checked on another host")
)

// quoteArg single-quotes a value that has already passed safeArgRE.
func quoteArg(v string) (string, error) {
	if !safeArgRE.MatchString(v) {
		return "", errUnsafeArg
	}
	return "'" + v + "'", nil
}

// remoteCommand is the command line a probe runs on another host. It is pure,
// so the table of hostile inputs can be tested without a network.
func remoteCommand(a Anchor) (string, error) {
	arg := func(k string) (string, error) { return quoteArg(a.Args[k]) }
	switch a.Type {
	case FileExists, DirExists, PathAbsent, RefPath:
		p, err := arg("path")
		if err != nil {
			return "", err
		}
		switch a.Type {
		case FileExists:
			return "test -f " + p, nil
		case DirExists:
			return "test -d " + p, nil
		case PathAbsent:
			return "test ! -e " + p, nil
		}
		return "test -e " + p + " -o -L " + p, nil
	case SymlinkTo:
		p, err := arg("path")
		if err != nil {
			return "", err
		}
		t, err := arg("target")
		if err != nil {
			return "", err
		}
		// Both paths are printed resolved; the verdict compares them here.
		return "test -L " + p + " && readlink -e " + p + " && readlink -e " + t, nil
	case FileContains:
		raw := a.Args["path"]
		if deniedPath(filepath.Clean(raw)) {
			return "", errDeniedPath
		}
		p, err := arg("path")
		if err != nil {
			return "", err
		}
		text, err := arg("text")
		if err != nil {
			return "", err
		}
		return "grep -F -q -e " + text + " -- " + p, nil
	case UnitActive, UnitInactive:
		u, err := arg("unit")
		if err != nil {
			return "", err
		}
		return "systemctl is-active " + u, nil
	case UnitEnabled:
		u, err := arg("unit")
		if err != nil {
			return "", err
		}
		return "systemctl is-enabled " + u, nil
	case RefUnit:
		u, err := arg("unit")
		if err != nil {
			return "", err
		}
		return "systemctl show -p LoadState --value " + u, nil
	case PortListening, PortClosed:
		return "ss -ltnH", nil
	case GitCommit:
		repo, err := arg("repo")
		if err != nil {
			return "", err
		}
		ref := a.Args["ref"]
		if !safeArgRE.MatchString(ref) {
			return "", errUnsafeArg
		}
		// ^{commit} is ours, not the writer's, and sits inside the quotes.
		return "git -C " + repo + " cat-file -e '" + ref + "^{commit}'", nil
	case GitBranch:
		repo, err := arg("repo")
		if err != nil {
			return "", err
		}
		b, err := quoteArg("refs/heads/" + a.Args["branch"])
		if err != nil {
			return "", err
		}
		return "git -C " + repo + " show-ref --verify --quiet " + b, nil
	}
	return "", errNotRemote
}

// runRemote checks an anchor on another host over ssh. A host outside the
// allowlist is never contacted.
func runRemote(ctx context.Context, a Anchor, host string) Result {
	target, ok := hostTarget(host)
	if !ok {
		return Result{Status: Skipped, Message: SkipHostNotAllowed}
	}
	if a.Type == BinaryVersion {
		return Result{Status: Skipped, Message: SkipBinaryRemote}
	}
	cmd, err := remoteCommand(a)
	switch {
	case errors.Is(err, errUnsafeArg):
		return Result{Status: Skipped, Message: SkipUnsafeArg}
	case err != nil:
		return Result{Status: Error, Message: err.Error()}
	}
	ctx, cancel := context.WithTimeout(ctx, remoteTimeout)
	defer cancel()
	out, code, err := sshRun(ctx, target, cmd)
	if err != nil {
		return Result{Status: Error, Message: err.Error()}
	}
	held, why, err := remoteVerdict(a, out, code)
	switch {
	case err != nil:
		return Result{Status: Error, Message: err.Error()}
	case held:
		return Result{Status: Pass}
	default:
		return Result{Status: Fail, Message: why}
	}
}

// sshRun runs one remote command and returns its stdout and exit status. ssh
// exiting 255 means it could not reach the host, which is an error, not a
// verdict on the claim.
func sshRun(ctx context.Context, target, command string) (string, int, error) {
	cmd := exec.CommandContext(ctx, sshBinary,
		"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new",
		target, "--", command)
	out, err := cmd.Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", 0, errors.New("remote probe timed out")
	}
	if err == nil {
		return string(out), 0, nil
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return "", 0, err
	}
	if ee.ExitCode() == 255 {
		return "", 255, errors.New("could not connect to the host")
	}
	return string(out), ee.ExitCode(), nil
}

// remoteVerdict reads a remote probe's answer. Test-style probes answer by exit
// status; systemctl and ss answer by their output, because systemctl exits
// non-zero for an ordinary "inactive".
func remoteVerdict(a Anchor, out string, code int) (bool, string, error) {
	state := strings.TrimSpace(out)
	switch a.Type {
	case FileExists:
		return code == 0, "missing or not a regular file", exitErr(code)
	case DirExists:
		return code == 0, "missing or not a directory", exitErr(code)
	case PathAbsent:
		return code == 0, "path exists", exitErr(code)
	case RefPath:
		return code == 0, "missing", exitErr(code)
	case FileContains:
		switch code {
		case 0:
			return true, "", nil
		case 1:
			return false, "text not present", nil
		case 2:
			return false, "missing or unreadable", nil
		}
		return false, "", exitErr(code)
	case SymlinkTo:
		if code == 1 {
			return false, "not a symlink or does not resolve", nil
		}
		if code != 0 {
			return false, "", exitErr(code)
		}
		lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
		if len(lines) < 2 {
			return false, "", errors.New("unexpected answer from the remote host")
		}
		return lines[0] == lines[1], "points elsewhere", nil
	case GitCommit, GitBranch:
		return code == 0, "not in repository", nil
	case UnitActive:
		if state == "" {
			return false, "", errors.New("systemctl gave no state")
		}
		return state == "active", "not active", nil
	case UnitInactive:
		if state == "" {
			return false, "", errors.New("systemctl gave no state")
		}
		return state != "active", "active", nil
	case UnitEnabled:
		if state == "" {
			return false, "", errors.New("systemctl gave no state")
		}
		switch state {
		case "enabled", "static", "enabled-runtime":
			return true, "", nil
		}
		return false, "not enabled", nil
	case RefUnit:
		return state != "" && state != "not-found", "unit not found", nil
	case PortListening, PortClosed:
		if code != 0 {
			return false, "", exitErr(code)
		}
		open := listeningIn(out, a.Args["port"])
		if a.Type == PortClosed {
			return !open, "listening", nil
		}
		return open, "not listening", nil
	}
	return false, "", errNotRemote
}

// exitErr turns an unexpected exit status into an error for the probe.
func exitErr(code int) error {
	if code == 0 || code == 1 {
		return nil
	}
	return errors.New("remote probe failed")
}
