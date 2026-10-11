package anchor

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Probe tests use only temp files, temp repositories, a fake systemctl on PATH
// and listeners this test opens itself. None of them depends on the units or
// ports of the host the tests run on.

// fakeSystemctl answers the three queries the runner makes, for three names:
// ok.service is active and enabled, user-only.service is active only in the
// user manager, and ghost.service does not exist.
const fakeSystemctl = `#!/bin/sh
user=0
if [ "$1" = "--user" ]; then user=1; shift; fi
verb=$1
for a; do unit=$a; done
case "$verb:$unit" in
  is-active:ok.service) echo active; exit 0 ;;
  is-active:user-only.service) if [ $user = 1 ]; then echo active; exit 0; fi; echo inactive; exit 3 ;;
  is-active:*) echo inactive; exit 3 ;;
  is-enabled:ok.service) echo enabled; exit 0 ;;
  is-enabled:static.service) echo static; exit 0 ;;
  is-enabled:*) echo disabled; exit 1 ;;
  show:ghost.service) echo not-found; exit 0 ;;
  show:*) echo loaded; exit 0 ;;
esac
echo unexpected; exit 2
`

const fakeBinary = "#!/bin/sh\necho \"grimoire 1.14.0 (test)\"\n"

// withFakeSystemctl puts a fake systemctl first on PATH for one test.
func withFakeSystemctl(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(fakeSystemctl), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// withFakeBinaries points binary_version at a temp directory.
func withFakeBinaries(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "grimoire"), []byte(fakeBinary), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binaryDir
	binaryDir = dir
	t.Cleanup(func() { binaryDir = old })
}

func mustRun(t *testing.T, a Anchor) Result {
	t.Helper()
	return Run(context.Background(), a)
}

func expect(t *testing.T, name string, got Result, want Status) {
	t.Helper()
	if got.Status != want {
		t.Errorf("%s: status %s (%q), want %s", name, got.Status, got.Message, want)
	}
}

func TestFileProbes(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(file, []byte("the router is at 192.168.0.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink(filepath.Join(dir, "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		a    Anchor
		want Status
	}{
		{"file exists", Anchor{Type: FileExists, Args: map[string]string{"path": file}}, Pass},
		{"file missing", Anchor{Type: FileExists, Args: map[string]string{"path": file + ".x"}}, Fail},
		{"file is not a dir", Anchor{Type: DirExists, Args: map[string]string{"path": file}}, Fail},
		{"dir exists", Anchor{Type: DirExists, Args: map[string]string{"path": dir}}, Pass},
		{"absent path is absent", Anchor{Type: PathAbsent, Args: map[string]string{"path": filepath.Join(dir, "gone")}}, Pass},
		{"present path is not absent", Anchor{Type: PathAbsent, Args: map[string]string{"path": file}}, Fail},
		{"symlink points at target", Anchor{Type: SymlinkTo, Args: map[string]string{"path": link, "target": file}}, Pass},
		{"symlink points elsewhere", Anchor{Type: SymlinkTo, Args: map[string]string{"path": link, "target": dir}}, Fail},
		{"a regular file is not a symlink", Anchor{Type: SymlinkTo, Args: map[string]string{"path": file, "target": file}}, Fail},
		{"file_contains finds literal", Anchor{Type: FileContains, Args: map[string]string{"path": file, "text": "192.168.0.1"}}, Pass},
		{"file_contains misses literal", Anchor{Type: FileContains, Args: map[string]string{"path": file, "text": "10.0.0.1"}}, Fail},
		{"file_contains on missing file", Anchor{Type: FileContains, Args: map[string]string{"path": file + ".x", "text": "x"}}, Fail},
		{"ref_path on dangling link", Anchor{Type: RefPath, Args: map[string]string{"path": dangling}}, Pass},
		{"ref_path on missing", Anchor{Type: RefPath, Args: map[string]string{"path": filepath.Join(dir, "gone")}}, Fail},
	}
	for _, c := range cases {
		expect(t, c.name, mustRun(t, c.a), c.want)
	}
	if r := mustRun(t, Anchor{Type: FileContains, Args: map[string]string{"path": dangling, "text": "x"}}); r.Status == Pass {
		t.Errorf("file_contains through a dangling link passed: %+v", r)
	}
}

func TestFileContainsReadsAtMostFiveMegabytes(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.txt")
	body := strings.Repeat("a", 6<<20) + "NEEDLE"
	if err := os.WriteFile(big, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, "needle past 5 MB", mustRun(t, Anchor{Type: FileContains,
		Args: map[string]string{"path": big, "text": "NEEDLE"}}), Fail)

	early := filepath.Join(dir, "early.txt")
	if err := os.WriteFile(early, []byte("NEEDLE"+strings.Repeat("a", 6<<20)), 0o644); err != nil {
		t.Fatal(err)
	}
	expect(t, "needle within 5 MB", mustRun(t, Anchor{Type: FileContains,
		Args: map[string]string{"path": early, "text": "NEEDLE"}}), Pass)
}

func TestFileContainsRefusesSecretPaths(t *testing.T) {
	dir := t.TempDir()
	vault := filepath.Join(dir, "vault")
	t.Setenv("GRIMOIRE_VAULT", vault)

	mk := func(rel, body string) string {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	secrets := mk(".ai-secrets/unifi-api.yaml", "X-API-KEY: hunter2\n")
	ssh := mk("home/.ssh/id_ed25519", "PRIVATE hunter2\n")
	env := mk("app/prod.env", "TOKEN=hunter2\n")
	seal := mk("var/seal-keys.json", "hunter2\n")
	shadow := mk("etc/shadow", "hunter2\n")
	index := mk("vault/.grimoire/index.db", "hunter2\n")
	inVault := mk("vault/notes/ok.md", "hunter2\n")
	link := filepath.Join(dir, "innocent-link")
	if err := os.Symlink(ssh, link); err != nil {
		t.Fatal(err)
	}
	noSuffix := mk("app/envfile", "hunter2\n")

	for name, p := range map[string]string{
		"ai-secrets dir": secrets, "ssh key": ssh, ".env file": env, "seal-keys": seal,
		"shadow": shadow, "vault index": index, "symlink into .ssh": link,
	} {
		r := mustRun(t, Anchor{Type: FileContains, Args: map[string]string{"path": p, "text": "hunter2"}})
		if r.Status != Error || r.Message != "denied path" {
			t.Errorf("%s: got %+v, want error denied path", name, r)
		}
	}
	// Two near misses that must still be readable.
	expect(t, "ordinary note in the vault", mustRun(t, Anchor{Type: FileContains,
		Args: map[string]string{"path": inVault, "text": "hunter2"}}), Pass)
	expect(t, "name without .env suffix", mustRun(t, Anchor{Type: FileContains,
		Args: map[string]string{"path": noSuffix, "text": "hunter2"}}), Pass)
}

func TestFileContainsRefusesNonRegularFiles(t *testing.T) {
	r := mustRun(t, Anchor{Type: FileContains, Args: map[string]string{"path": os.DevNull, "text": "x"}})
	if r.Status == Pass {
		t.Fatalf("a device was read: %+v", r)
	}
}

func TestUnitProbesUseTheSystemThenTheUserManager(t *testing.T) {
	withFakeSystemctl(t)
	cases := []struct {
		name string
		a    Anchor
		want Status
	}{
		{"system unit active", Anchor{Type: UnitActive, Args: map[string]string{"unit": "ok.service"}}, Pass},
		{"user unit active", Anchor{Type: UnitActive, Args: map[string]string{"unit": "user-only.service"}}, Pass},
		{"down unit not active", Anchor{Type: UnitActive, Args: map[string]string{"unit": "down.service"}}, Fail},
		{"down unit inactive", Anchor{Type: UnitInactive, Args: map[string]string{"unit": "down.service"}}, Pass},
		{"running unit is not inactive", Anchor{Type: UnitInactive, Args: map[string]string{"unit": "ok.service"}}, Fail},
		{"user-active unit is not inactive", Anchor{Type: UnitInactive, Args: map[string]string{"unit": "user-only.service"}}, Fail},
		{"enabled", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "ok.service"}}, Pass},
		{"static counts as enabled", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "static.service"}}, Pass},
		{"disabled", Anchor{Type: UnitEnabled, Args: map[string]string{"unit": "off.service"}}, Fail},
		{"ref_unit found", Anchor{Type: RefUnit, Args: map[string]string{"unit": "ok.service"}}, Pass},
		{"ref_unit not found", Anchor{Type: RefUnit, Args: map[string]string{"unit": "ghost.service"}}, Fail},
	}
	for _, c := range cases {
		expect(t, c.name, mustRun(t, c.a), c.want)
	}
}

func TestPortProbesReadTheListeningSocket(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("ss is not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	expect(t, "our listener is listening", mustRun(t, Anchor{Type: PortListening,
		Args: map[string]string{"port": port}}), Pass)
	expect(t, "our listener is not closed", mustRun(t, Anchor{Type: PortClosed,
		Args: map[string]string{"port": port}}), Fail)
	l.Close()
	expect(t, "closed port is closed", mustRun(t, Anchor{Type: PortClosed,
		Args: map[string]string{"port": port}}), Pass)
}

func TestHTTPProbesStayOnLoopbackGetOnlyAndDoNotFollowRedirects(t *testing.T) {
	big := strings.Repeat("x", 300<<10) + "TAIL"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte("hello from the probe"))
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		case "/big":
			w.Write([]byte(big))
		case "/post-only":
			if r.Method != http.MethodGet {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case "/missing":
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	base := srv.URL

	cases := []struct {
		name string
		a    Anchor
		want Status
	}{
		{"status matches", Anchor{Type: HTTPStatus, Args: map[string]string{"url": base + "/ok", "status": "200"}}, Pass},
		{"status differs", Anchor{Type: HTTPStatus, Args: map[string]string{"url": base + "/missing", "status": "200"}}, Fail},
		{"redirect is not followed", Anchor{Type: HTTPStatus, Args: map[string]string{"url": base + "/redirect", "status": "302"}}, Pass},
		{"redirect target not reached", Anchor{Type: HTTPStatus, Args: map[string]string{"url": base + "/redirect", "status": "200"}}, Fail},
		{"text found", Anchor{Type: HTTPContains, Args: map[string]string{"url": base + "/ok", "text": "the probe"}}, Pass},
		{"text absent", Anchor{Type: HTTPContains, Args: map[string]string{"url": base + "/ok", "text": "nope"}}, Fail},
		{"error page is not a claim", Anchor{Type: HTTPContains, Args: map[string]string{"url": base + "/missing", "text": "404"}}, Fail},
		{"body read is capped at 200 KB", Anchor{Type: HTTPContains, Args: map[string]string{"url": base + "/big", "text": "TAIL"}}, Fail},
	}
	for _, c := range cases {
		expect(t, c.name, mustRun(t, c.a), c.want)
	}
}

func TestBinaryVersionRunsOnlyTheNamedBinary(t *testing.T) {
	withFakeBinaries(t)
	expect(t, "version text found", mustRun(t, Anchor{Type: BinaryVersion,
		Args: map[string]string{"bin": "grimoire", "text": "1.14.0"}}), Pass)
	expect(t, "version text differs", mustRun(t, Anchor{Type: BinaryVersion,
		Args: map[string]string{"bin": "grimoire", "text": "2.0.0"}}), Fail)
	// lectern has no fake here, so the fixed path does not exist: an error,
	// never a search of PATH for some other program.
	if r := mustRun(t, Anchor{Type: BinaryVersion, Args: map[string]string{"bin": "lectern", "text": "x"}}); r.Status != Error {
		t.Fatalf("missing binary = %+v, want error", r)
	}
	if r := mustRun(t, Anchor{Type: BinaryVersion, Args: map[string]string{"bin": "sh", "text": "x"}}); r.Status != Error {
		t.Fatalf("a non-allowed binary was run: %+v", r)
	}
}

func TestGitProbesReadARealRepository(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q", "-b", "main")
	run("commit", "-q", "--allow-empty", "-m", "first")
	run("tag", "v1.0.0")
	run("branch", "lab/ground")

	expect(t, "commit exists", mustRun(t, Anchor{Type: GitCommit, Args: map[string]string{"repo": repo, "ref": "main"}}), Pass)
	expect(t, "tag exists", mustRun(t, Anchor{Type: GitCommit, Args: map[string]string{"repo": repo, "ref": "v1.0.0"}}), Pass)
	expect(t, "commit missing", mustRun(t, Anchor{Type: GitCommit, Args: map[string]string{"repo": repo, "ref": "v9"}}), Fail)
	expect(t, "branch exists", mustRun(t, Anchor{Type: GitBranch, Args: map[string]string{"repo": repo, "branch": "lab/ground"}}), Pass)
	expect(t, "branch missing", mustRun(t, Anchor{Type: GitBranch, Args: map[string]string{"repo": repo, "branch": "nope"}}), Fail)
}

func TestRemoteIsSkippedAndInvalidAnchorsAreErrorsNotProbes(t *testing.T) {
	expect(t, "remote", mustRun(t, Anchor{Type: Remote, Args: map[string]string{"what": "https://example.com/x"}}), Skipped)
	r := mustRun(t, Anchor{Type: "shell", Args: map[string]string{"cmd": "ls"}})
	if r.Status != Error || r.Message == "" {
		t.Fatalf("an unknown type = %+v, want error with a message", r)
	}
}

func TestRunNeverExecutesAShell(t *testing.T) {
	// A path made of shell syntax is a file name, not a command.
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	evil := filepath.Join(dir, "x; touch "+marker)
	expect(t, "shell metacharacters stay in the name", mustRun(t, Anchor{Type: FileExists,
		Args: map[string]string{"path": evil}}), Fail)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a probe argument ran as a shell command")
	}
}
