package anchor

import (
	"strings"
	"testing"
)

func TestParseHostsReadsNameTargetPairs(t *testing.T) {
	got, err := ParseHosts("mediaserver=root@100.64.0.10, Docker-Server=root@100.64.0.11,")
	if err != nil {
		t.Fatal(err)
	}
	if got["mediaserver"] != "root@100.64.0.10" || got["docker-server"] != "root@100.64.0.11" || len(got) != 2 {
		t.Fatalf("parsed = %v", got)
	}
	if empty, err := ParseHosts(""); err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v, %v; want an empty allowlist", empty, err)
	}
}

func TestParseHostsRefusesWhatCouldBecomeAnOption(t *testing.T) {
	for _, bad := range []string{"mediaserver", "=root@x", "bad name=root@x", "a=-oProxyCommand=x",
		"a=root@x y", "a=", "-x=root@x", "a=root@x\nb"} {
		if _, err := ParseHosts(bad); err == nil {
			t.Errorf("ParseHosts(%q) accepted", bad)
		}
	}
}

func TestInheritGivesAFactsHostToAnchorsThatNameNone(t *testing.T) {
	localHostFn = func() string { return "aiserver" }
	defer func() { localHostFn = shortHostname }()
	as := []Anchor{
		{Type: UnitActive, Args: map[string]string{"unit": "a.service"}},
		{Type: FileExists, Args: map[string]string{"path": "/srv/x", "host": "docker-server"}},
	}
	out := Inherit(as, "MediaServer")
	if out[0].Args["host"] != "mediaserver" || out[1].Args["host"] != "docker-server" {
		t.Fatalf("inherited = %v", out)
	}
	if _, ok := as[0].Args["host"]; ok {
		t.Fatal("Inherit changed the caller's anchor")
	}
	if same := Inherit(as, "aiserver"); same[0].Args["host"] != "" {
		t.Fatal("a fact about this machine gave its anchors a host")
	}
	if same := Inherit(as, ""); same[0].Args["host"] != "" {
		t.Fatal("a fact with no host gave its anchors a host")
	}
}

func TestValidateAcceptsHostAndAutoOnlyWhereMeant(t *testing.T) {
	ok := []Anchor{
		{Type: FileExists, Args: map[string]string{"path": "/a", "host": "mediaserver"}},
		AutoAnchor(Anchor{Type: RefPath, Args: map[string]string{"path": "/a"}}),
		AutoAnchor(Anchor{Type: RefUnit, Args: map[string]string{"unit": "a.service"}}),
	}
	for _, a := range ok {
		if err := Validate(a); err != nil {
			t.Errorf("Validate(%v): %v", a, err)
		}
	}
	bad := []Anchor{
		{Type: FileExists, Args: map[string]string{"path": "/a", "host": "bad host"}},
		{Type: FileExists, Args: map[string]string{"path": "/a", "auto": "1"}},
		{Type: RefPath, Args: map[string]string{"path": "/a", "auto": "2"}},
	}
	for _, a := range bad {
		if err := Validate(a); err == nil {
			t.Errorf("Validate(%v) accepted", a)
		}
	}
	if Storable(RefPath) || !Storable(FileExists) {
		t.Fatal("Storable changed: reference types must stay refused for writers")
	}
	if !IsAuto(AutoAnchor(Anchor{Type: RefPath, Args: map[string]string{"path": "/a"}})) {
		t.Fatal("AutoAnchor is not marked auto")
	}
	if !strings.Contains(Encode([]Anchor{AutoAnchor(Anchor{Type: RefPath, Args: map[string]string{"path": "/a"}})}), "auto=1") {
		t.Fatal("auto mark does not survive encoding")
	}
}
