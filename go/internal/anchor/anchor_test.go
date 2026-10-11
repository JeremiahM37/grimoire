package anchor

import (
	"reflect"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTripsAwkwardCharacters(t *testing.T) {
	cases := [][]Anchor{
		nil,
		{{Type: PortClosed, Args: map[string]string{"port": "9111"}}},
		{{Type: FileContains, Args: map[string]string{
			"path": "/home/admin/with space/notes (copy).md",
			"text": "a = b | c; d % e --> f",
		}}},
		{{Type: FileContains, Args: map[string]string{
			"path": "/home/admin/café/naïve 日本.md",
			"text": "→ \"quoted\" \\back\\slash\ttab\nnewline>gt<lt",
		}}},
		{{Type: UnitActive, Args: map[string]string{"unit": "grimoire.service"}},
			{Type: HTTPContains, Args: map[string]string{"url": "http://127.0.0.1:9111/api/health", "text": "ok"}}},
		{{Type: RefPath, Args: map[string]string{"path": "/a%2Fb/%%"}}},
		{{Type: Remote, Args: map[string]string{"what": "pve=é|x;y"}}},
	}
	for i, in := range cases {
		enc := Encode(in)
		if strings.ContainsAny(enc, " \t\r\n<>\"\\") || strings.Contains(enc, "-->") {
			t.Errorf("case %d: encoding %q is not safe inside a trailer", i, enc)
		}
		out, err := Decode(enc)
		if err != nil {
			t.Fatalf("case %d: decode %q: %v", i, enc, err)
		}
		if len(in) == 0 && out == nil {
			continue
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("case %d: round trip changed the anchors:\n in  %#v\n out %#v", i, in, out)
		}
		if again := Encode(out); again != enc {
			t.Errorf("case %d: re-encoding is not byte-identical: %q != %q", i, again, enc)
		}
	}
}

func TestEncodeIsDeterministicInArgumentOrder(t *testing.T) {
	a := Anchor{Type: FileContains, Args: map[string]string{"text": "t", "path": "/p"}}
	b := Anchor{Type: FileContains, Args: map[string]string{"path": "/p", "text": "t"}}
	if Encode([]Anchor{a}) != Encode([]Anchor{b}) {
		t.Fatalf("same anchor encoded two ways: %q vs %q", Encode([]Anchor{a}), Encode([]Anchor{b}))
	}
	if got := Encode([]Anchor{a}); got != "file_contains;path=/p;text=t" {
		t.Fatalf("encoding = %q", got)
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	for _, bad := range []string{"|", "a||b", ";path=/x", "file_contains;path", "file_contains;=x",
		"file_contains;path=/a;path=/b", "file_contains;path=%ZZ", "file_contains;path=%4", "file_contains;path=%"} {
		if _, err := Decode(bad); err == nil {
			t.Errorf("Decode(%q) accepted malformed input", bad)
		}
	}
}

func TestValidateAcceptsEveryProbeTypeAndRefusesBadArguments(t *testing.T) {
	good := []Anchor{
		{Type: FileExists, Args: map[string]string{"path": "/etc/hostname"}},
		{Type: DirExists, Args: map[string]string{"path": "/tmp"}},
		{Type: PathAbsent, Args: map[string]string{"path": "/nonexistent/x"}},
		{Type: SymlinkTo, Args: map[string]string{"path": "/a", "target": "/b"}},
		{Type: FileContains, Args: map[string]string{"path": "/etc/hostname", "text": "x"}},
		{Type: UnitActive, Args: map[string]string{"unit": "grimoire.service"}},
		{Type: UnitInactive, Args: map[string]string{"unit": "x@1.service"}},
		{Type: UnitEnabled, Args: map[string]string{"unit": "lectern.service"}},
		{Type: PortListening, Args: map[string]string{"port": "9111"}},
		{Type: PortClosed, Args: map[string]string{"port": "65535"}},
		{Type: HTTPStatus, Args: map[string]string{"url": "http://127.0.0.1:9111/", "status": "200"}},
		{Type: HTTPContains, Args: map[string]string{"url": "http://[::1]:9111/", "text": "x"}},
		{Type: HTTPContains, Args: map[string]string{"url": "https://100.96.103.31:8444/", "text": "x"}},
		{Type: BinaryVersion, Args: map[string]string{"bin": "lectern", "text": "1."}},
		{Type: GitCommit, Args: map[string]string{"repo": "/srv/x", "ref": "v1.14.0"}},
		{Type: GitBranch, Args: map[string]string{"repo": "/srv/x", "branch": "lab/ground"}},
		{Type: Remote, Args: map[string]string{"what": "https://example.com"}},
	}
	for _, a := range good {
		if err := Validate(a); err != nil {
			t.Errorf("Validate(%s %v) = %v, want nil", a.Type, a.Args, err)
		}
	}
	bad := []Anchor{
		{Type: "shell", Args: map[string]string{"cmd": "ls"}},
		{Type: FileExists},
		{Type: FileExists, Args: map[string]string{"path": ""}},
		{Type: FileExists, Args: map[string]string{"path": "relative/x"}},
		{Type: FileExists, Args: map[string]string{"path": "/x", "extra": "1"}},
		{Type: SymlinkTo, Args: map[string]string{"path": "/a", "target": "b"}},
		{Type: FileContains, Args: map[string]string{"path": "/x", "text": "a\nb"}},
		{Type: UnitActive, Args: map[string]string{"unit": "bad name"}},
		{Type: UnitActive, Args: map[string]string{"unit": "-x.service"}},
		{Type: UnitActive, Args: map[string]string{"unit": "a;rm.service"}},
		{Type: PortListening, Args: map[string]string{"port": "0"}},
		{Type: PortListening, Args: map[string]string{"port": "65536"}},
		{Type: PortListening, Args: map[string]string{"port": "+80"}},
		{Type: PortListening, Args: map[string]string{"port": "80x"}},
		{Type: HTTPStatus, Args: map[string]string{"url": "http://localhost:9111/", "status": "200"}},
		{Type: HTTPStatus, Args: map[string]string{"url": "http://8.8.8.8/", "status": "200"}},
		{Type: HTTPStatus, Args: map[string]string{"url": "ftp://127.0.0.1/", "status": "200"}},
		{Type: HTTPStatus, Args: map[string]string{"url": "http://127.0.0.1/", "status": "700"}},
		{Type: BinaryVersion, Args: map[string]string{"bin": "bash", "text": "x"}},
		{Type: BinaryVersion, Args: map[string]string{"bin": "/bin/sh", "text": "x"}},
		{Type: GitCommit, Args: map[string]string{"repo": "/srv/x", "ref": "-evil"}},
		{Type: GitCommit, Args: map[string]string{"repo": "/srv/x", "ref": "a..b"}},
		{Type: GitBranch, Args: map[string]string{"repo": "relative", "branch": "main"}},
		{Type: Remote},
	}
	for _, a := range bad {
		if err := Validate(a); err == nil {
			t.Errorf("Validate(%s %v) accepted a bad anchor", a.Type, a.Args)
		}
	}
}

func TestStorableRefusesReferenceOnlyTypes(t *testing.T) {
	if Storable(RefPath) || Storable(RefUnit) || Storable("nope") {
		t.Fatal("a reference-only or unknown type was storable")
	}
	if !Storable(FileContains) || !Storable(Remote) {
		t.Fatal("a probe type was not storable")
	}
}

func TestFromMapReadsStringsAndNumbers(t *testing.T) {
	a, err := FromMap(map[string]any{"type": "port_listening", "port": float64(9111)})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != PortListening || a.Args["port"] != "9111" {
		t.Fatalf("got %#v", a)
	}
	if _, err := FromMap(map[string]any{"path": "/x"}); err == nil {
		t.Error("a map with no type was accepted")
	}
	if _, err := FromMap(map[string]any{"type": "file_exists", "path": []any{"/x"}}); err == nil {
		t.Error("a list-valued argument was accepted")
	}
}
