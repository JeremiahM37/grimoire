package anchor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// These tests never open a network connection. ssh is replaced by a fake
// script that records its arguments and answers with a chosen exit status and
// output, and the allowlist is set to a target that is never dialled.

// fakeSSH points sshBinary at a script that writes its argv to a file, prints
// FAKE_OUT and exits with FAKE_CODE. It returns the argv file's path.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$FAKE_ARGS\"\nprintf '%s' \"$FAKE_OUT\"\nexit \"$FAKE_CODE\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	argv := filepath.Join(dir, "argv")
	old := sshBinary
	sshBinary = bin
	t.Cleanup(func() { sshBinary = old })
	t.Setenv("FAKE_ARGS", argv)
	return argv
}

// allowlist sets one host and this machine's name for one test.
func allowlist(t *testing.T, local string) {
	t.Helper()
	SetHosts(map[string]string{"mediaserver": "root@example.invalid"})
	oldLocal := localHostFn
	localHostFn = func() string { return local }
	t.Cleanup(func() {
		SetHosts(nil)
		localHostFn = oldLocal
	})
}

func ssh(t *testing.T, code int, out string) string {
	t.Helper()
	argv := fakeSSH(t)
	t.Setenv("FAKE_CODE", strconv.Itoa(code))
	t.Setenv("FAKE_OUT", out)
	return argv
}

func argvWasCalled(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRemoteCommandBuildsFixedCommandsFromSafeArguments(t *testing.T) {
	cases := []struct {
		name string
		a    Anchor
		want string
	}{
		{"file_exists", Anchor{Type: FileExists, Args: map[string]string{"path": "/srv/app/cfg.yaml"}},
			"test -f '/srv/app/cfg.yaml'"},
		{"dir_exists", Anchor{Type: DirExists, Args: map[string]string{"path": "/srv/app"}},
			"test -d '/srv/app'"},
		{"path_absent", Anchor{Type: PathAbsent, Args: map[string]string{"path": "/srv/old"}},
			"test ! -e '/srv/old'"},
		{"file_contains", Anchor{Type: FileContains, Args: map[string]string{"path": "/srv/lan.conf", "text": "ROUTER_IP=192.168.0.1"}},
			"grep -F -q -e 'ROUTER_IP=192.168.0.1' -- '/srv/lan.conf'"},
		{"symlink_to", Anchor{Type: SymlinkTo, Args: map[string]string{"path": "/srv/current", "target": "/srv/releases/9"}},
			"test -L '/srv/current' && readlink -e '/srv/current' && readlink -e '/srv/releases/9'"},
		{"unit_active", Anchor{Type: UnitActive, Args: map[string]string{"unit": "grimoire.service"}},
			"systemctl is-active 'grimoire.service'"},
		{"unit_inactive", Anchor{Type: UnitInactive, Args: map[string]string{"unit": "old.timer"}},
			"systemctl is-active 'old.timer'"},
		{"unit_enabled", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "grimoire.service"}},
			"systemctl is-enabled 'grimoire.service'"},
		{"port_listening", Anchor{Type: PortListening, Args: map[string]string{"port": "9111"}}, "ss -ltnH"},
		{"port_closed", Anchor{Type: PortClosed, Args: map[string]string{"port": "65000"}}, "ss -ltnH"},
		{"git_commit", Anchor{Type: GitCommit, Args: map[string]string{"repo": "/srv/repo", "ref": "v1.2.3"}},
			"git -C '/srv/repo' cat-file -e 'v1.2.3^{commit}'"},
		{"git_branch", Anchor{Type: GitBranch, Args: map[string]string{"repo": "/srv/repo", "branch": "main"}},
			"git -C '/srv/repo' show-ref --verify --quiet 'refs/heads/main'"},
		{"ref_path", Anchor{Type: RefPath, Args: map[string]string{"path": "/srv/notes.md"}},
			"test -e '/srv/notes.md' -o -L '/srv/notes.md'"},
		{"ref_unit", Anchor{Type: RefUnit, Args: map[string]string{"unit": "caddy.service"}},
			"systemctl show -p LoadState --value 'caddy.service'"},
	}
	for _, c := range cases {
		got, err := remoteCommand(c.a)
		if err != nil || got != c.want {
			t.Errorf("%s: remoteCommand = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
}

// Every hostile value must be refused as unsafe, never quoted and sent.
func TestRemoteCommandRefusesHostileArguments(t *testing.T) {
	hostile := []string{
		"/tmp/a; rm -rf /",
		"/tmp/$(id)",
		"/tmp/`id`",
		"/tmp/a'b",
		"/tmp/a\"b",
		"/tmp/a\nb",
		"/tmp/a b",
		"~/notes.md",
		"/tmp/*.txt",
		"/tmp/a|b",
		"/tmp/a&b",
		"",
	}
	for _, p := range hostile {
		for _, typ := range []string{FileExists, DirExists, PathAbsent, SymlinkTo, FileContains, RefPath} {
			args := map[string]string{"path": p, "target": "/srv/x", "text": "ok"}
			if _, err := remoteCommand(Anchor{Type: typ, Args: args}); err == nil {
				t.Errorf("%s with path %q built a remote command", typ, p)
			}
		}
	}
	for _, text := range []string{"x y", "a'b", "$(id)", "a;b", "a\nb", "a`b`"} {
		_, err := remoteCommand(Anchor{Type: FileContains, Args: map[string]string{"path": "/srv/a", "text": text}})
		if !errors.Is(err, errUnsafeArg) {
			t.Errorf("file_contains text %q: err = %v, want unsafe", text, err)
		}
	}
	for _, u := range []string{"x; reboot", "$(id).service", "a b.service"} {
		_, err := remoteCommand(Anchor{Type: UnitActive, Args: map[string]string{"unit": u}})
		if !errors.Is(err, errUnsafeArg) {
			t.Errorf("unit %q: err = %v, want unsafe", u, err)
		}
	}
	_, err := remoteCommand(Anchor{Type: GitCommit, Args: map[string]string{"repo": "/srv/r", "ref": "v1;reboot"}})
	if !errors.Is(err, errUnsafeArg) {
		t.Errorf("git ref with ';': err = %v, want unsafe", err)
	}
}

func TestRemoteCommandRefusesDeniedFilesAndUnsupportedTypes(t *testing.T) {
	for _, p := range []string{"/home/admin/.ai-secrets/x.yaml", "/srv/app/.env", "/root/.ssh/id_ed25519"} {
		_, err := remoteCommand(Anchor{Type: FileContains, Args: map[string]string{"path": p, "text": "x"}})
		if !errors.Is(err, errDeniedPath) {
			t.Errorf("file_contains %s: err = %v, want denied", p, err)
		}
	}
	for _, typ := range []string{HTTPStatus, HTTPContains, BinaryVersion, Remote} {
		if _, err := remoteCommand(Anchor{Type: typ, Args: map[string]string{}}); !errors.Is(err, errNotRemote) {
			t.Errorf("%s: err = %v, want not remote", typ, err)
		}
	}
}

func TestRemoteProbeExitStatusMapsToVerdicts(t *testing.T) {
	allowlist(t, "aiserver")
	a := Anchor{Type: FileExists, Args: map[string]string{"path": "/srv/app/cfg.yaml", "host": "mediaserver"}}

	ssh(t, 0, "")
	if res := Run(context.Background(), a); res.Status != Pass {
		t.Errorf("exit 0: %+v, want pass", res)
	}
	ssh(t, 1, "")
	if res := Run(context.Background(), a); res.Status != Fail {
		t.Errorf("exit 1: %+v, want fail", res)
	}
	ssh(t, 255, "")
	if res := Run(context.Background(), a); res.Status != Error {
		t.Errorf("exit 255 (could not connect): %+v, want error", res)
	}
}

func TestRemoteRunPassesArgvWithoutAShellAndWithBatchOptions(t *testing.T) {
	allowlist(t, "aiserver")
	argv := ssh(t, 0, "")
	Run(context.Background(), Anchor{Type: DirExists, Args: map[string]string{"path": "/srv/app", "host": "mediaserver"}})
	raw, err := os.ReadFile(argv)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "-o", "StrictHostKeyChecking=accept-new",
		"root@example.invalid", "--", "test -d '/srv/app'"}
	if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("ssh argv = %q, want %q", args, want)
	}
}

func TestRemoteSkipsWithoutContactingAnyHost(t *testing.T) {
	allowlist(t, "aiserver")
	argv := ssh(t, 0, "")

	res := Run(context.Background(), Anchor{Type: FileExists, Args: map[string]string{"path": "/srv/a", "host": "unknown-box"}})
	if res.Status != Skipped || res.Message != SkipHostNotAllowed {
		t.Errorf("host not allowlisted: %+v", res)
	}
	res = Run(context.Background(), Anchor{Type: FileExists, Args: map[string]string{"path": "/srv/a; id", "host": "mediaserver"}})
	if res.Status != Skipped || res.Message != SkipUnsafeArg {
		t.Errorf("unsafe argument: %+v", res)
	}
	res = Run(context.Background(), Anchor{Type: BinaryVersion, Args: map[string]string{"bin": "grimoire", "text": "1.15", "host": "mediaserver"}})
	if res.Status != Skipped || res.Message != SkipBinaryRemote {
		t.Errorf("binary_version on another host: %+v", res)
	}
	res = Run(context.Background(), Anchor{Type: Remote, Args: map[string]string{"what": "https://example.com", "host": "mediaserver"}})
	if res.Status != Skipped || res.Message != SkipRemote {
		t.Errorf("remote type: %+v", res)
	}
	if argvWasCalled(argv) {
		t.Fatal("ssh was run for an anchor that must be skipped")
	}
}

func TestHTTPProbesRunHereWhateverTheHost(t *testing.T) {
	allowlist(t, "aiserver")
	argv := ssh(t, 0, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("grimoire ok"))
	}))
	defer srv.Close()
	res := Run(context.Background(), Anchor{Type: HTTPContains, Args: map[string]string{"url": srv.URL, "text": "grimoire ok", "host": "mediaserver"}})
	if res.Status != Pass {
		t.Fatalf("http_contains with a host: %+v, want pass from here", res)
	}
	if argvWasCalled(argv) {
		t.Fatal("an http probe was sent over ssh")
	}
}

func TestAnchorNamingThisMachineRunsLocally(t *testing.T) {
	allowlist(t, "aiserver")
	argv := ssh(t, 0, "")
	f := filepath.Join(t.TempDir(), "here.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Anchor{Type: FileExists, Args: map[string]string{"path": f, "host": "AISERVER"}})
	if res.Status != Pass {
		t.Fatalf("local host: %+v, want pass", res)
	}
	if argvWasCalled(argv) {
		t.Fatal("a probe for this machine went over ssh")
	}
}

func TestRemoteVerdictsReadTheirOwnAnswers(t *testing.T) {
	allowlist(t, "aiserver")
	cases := []struct {
		name string
		a    Anchor
		code int
		out  string
		want Status
	}{
		{"unit active", Anchor{Type: UnitActive, Args: map[string]string{"unit": "grimoire.service", "host": "mediaserver"}}, 0, "active\n", Pass},
		{"unit inactive exits 3", Anchor{Type: UnitActive, Args: map[string]string{"unit": "grimoire.service", "host": "mediaserver"}}, 3, "inactive\n", Fail},
		{"unit inactive claim holds", Anchor{Type: UnitInactive, Args: map[string]string{"unit": "old.service", "host": "mediaserver"}}, 3, "inactive\n", Pass},
		{"unit enabled static", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "x.service", "host": "mediaserver"}}, 0, "static\n", Pass},
		{"unit disabled", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "x.service", "host": "mediaserver"}}, 1, "disabled\n", Fail},
		{"unit no answer", Anchor{Type: UnitActive, Args: map[string]string{"unit": "x.service", "host": "mediaserver"}}, 1, "", Error},
		{"symlink same", Anchor{Type: SymlinkTo, Args: map[string]string{"path": "/a", "target": "/b", "host": "mediaserver"}}, 0, "/srv/9\n/srv/9\n", Pass},
		{"symlink elsewhere", Anchor{Type: SymlinkTo, Args: map[string]string{"path": "/a", "target": "/b", "host": "mediaserver"}}, 0, "/srv/8\n/srv/9\n", Fail},
		{"symlink not a link", Anchor{Type: SymlinkTo, Args: map[string]string{"path": "/a", "target": "/b", "host": "mediaserver"}}, 1, "", Fail},
		{"port listening", Anchor{Type: PortListening, Args: map[string]string{"port": "9111", "host": "mediaserver"}}, 0,
			"LISTEN 0 4096 127.0.0.1:9111 0.0.0.0:*\n", Pass},
		{"port closed", Anchor{Type: PortClosed, Args: map[string]string{"port": "9111", "host": "mediaserver"}}, 0,
			"LISTEN 0 4096 127.0.0.1:9112 0.0.0.0:*\n", Pass},
		{"grep finds", Anchor{Type: FileContains, Args: map[string]string{"path": "/srv/a", "text": "x", "host": "mediaserver"}}, 0, "", Pass},
		{"grep absent", Anchor{Type: FileContains, Args: map[string]string{"path": "/srv/a", "text": "x", "host": "mediaserver"}}, 1, "", Fail},
		{"git commit missing", Anchor{Type: GitCommit, Args: map[string]string{"repo": "/srv/r", "ref": "v9", "host": "mediaserver"}}, 128, "", Fail},
		{"ref unit not found", Anchor{Type: RefUnit, Args: map[string]string{"unit": "gone.service", "host": "mediaserver"}}, 0, "not-found\n", Fail},
		{"ref unit found", Anchor{Type: RefUnit, Args: map[string]string{"unit": "caddy.service", "host": "mediaserver"}}, 0, "loaded\n", Pass},
	}
	for _, c := range cases {
		ssh(t, c.code, c.out)
		if res := Run(context.Background(), c.a); res.Status != c.want {
			t.Errorf("%s: %+v, want %s", c.name, res, c.want)
		}
	}
}

func TestSkipReasonNamesTheThreeKinds(t *testing.T) {
	cases := map[string]string{SkipRemote: "remote", SkipHostNotAllowed: "host_not_allowlisted",
		SkipUnsafeArg: "unsafe_argument", SkipBinaryRemote: "other"}
	for msg, want := range cases {
		if got := SkipReason(Result{Status: Skipped, Message: msg}); got != want {
			t.Errorf("SkipReason(%q) = %q, want %q", msg, got, want)
		}
	}
}
