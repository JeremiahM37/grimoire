package anchor

import (
	"testing"
)

func TestExtractRefsFindsPathsAndUnits(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	text := "Run ~/projects/inference-research and /mnt/bulk/gguf/model.gguf, " +
		"see /home/admin/notes/x.md. The unit grimoire.service and lectern-web-mcp.timer run it.\n" +
		"Then ~/projects/inference-research again."
	got := ExtractRefs(text)
	want := []Anchor{
		{Type: RefPath, Args: map[string]string{"path": "/home/tester/projects/inference-research"}},
		{Type: RefPath, Args: map[string]string{"path": "/mnt/bulk/gguf/model.gguf"}},
		{Type: RefPath, Args: map[string]string{"path": "/home/admin/notes/x.md"}},
		{Type: RefUnit, Args: map[string]string{"unit": "grimoire.service"}},
		{Type: RefUnit, Args: map[string]string{"unit": "lectern-web-mcp.timer"}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d refs, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Type != want[i].Type || got[i].Args["path"] != want[i].Args["path"] || got[i].Args["unit"] != want[i].Args["unit"] {
			t.Errorf("ref %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, a := range got {
		if err := Validate(a); err != nil {
			t.Errorf("extracted ref %+v does not validate: %v", a, err)
		}
	}
}

func TestExtractRefsSkipsTemplatesGlobsAndBareRoots(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	text := "glob /mnt/bulk/cache/*.json, template /home/admin/{name}/x, " +
		"angle /home/admin/<x>, dollar /home/admin/$HOME/y, roots /opt/ /etc /mnt/bulk /home/admin/ ~/, " +
		"instance getty@tty1.service and -flag.service."
	if got := ExtractRefs(text); len(got) != 0 {
		t.Fatalf("expected no refs, got %+v", got)
	}
}

func TestExtractRefsRespectsTheCharacterBeforeAMatch(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	for _, text := range []string{"x/home/admin/notes/a.md", "a.~/projects/b", "_/mnt/bulk/c"} {
		if got := ExtractRefs(text); len(got) != 0 {
			t.Errorf("%q produced refs %+v; the match is inside a longer token", text, got)
		}
	}
}

func TestExtractRefsDeduplicates(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	got := ExtractRefs("/mnt/bulk/a.txt /mnt/bulk/a.txt grimoire.service grimoire.service")
	if len(got) != 2 {
		t.Fatalf("got %d refs, want 2: %+v", len(got), got)
	}
}

func TestExtractRefsWithoutAHomeSkipsTildePaths(t *testing.T) {
	t.Setenv("HOME", "")
	if got := ExtractRefs("~/projects/x and /mnt/bulk/y"); len(got) != 1 || got[0].Args["path"] != "/mnt/bulk/y" {
		t.Fatalf("got %+v, want only the absolute path", got)
	}
}

// System paths are references too, and a unit named inside one is part of the
// path, never a unit of its own.
func TestExtractRefsTakesSystemPathsAndNotUnitsFromInsidePaths(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	cases := []struct {
		name string
		text string
		want []Anchor
	}{
		{"systemd drop-in path, no unit",
			"reads /etc/systemd/system/grimoire.service.d/release.conf to find the web directory",
			[]Anchor{{Type: RefPath, Args: map[string]string{"path": "/etc/systemd/system/grimoire.service.d/release.conf"}}}},
		{"unit file path, no unit",
			"see /etc/systemd/system/grimoire.service",
			[]Anchor{{Type: RefPath, Args: map[string]string{"path": "/etc/systemd/system/grimoire.service"}}}},
		{"unit name in prose is still a unit",
			"restart grimoire.service after the edit",
			[]Anchor{{Type: RefUnit, Args: map[string]string{"unit": "grimoire.service"}}}},
		{"opt, usr/local, var/lib, var/log and srv roots",
			"/opt/docker/x.yml /usr/local/bin/grimoire /var/lib/grimoire-seal/seal-keys.json /var/log/syslog /srv/www/app.conf",
			[]Anchor{
				{Type: RefPath, Args: map[string]string{"path": "/opt/docker/x.yml"}},
				{Type: RefPath, Args: map[string]string{"path": "/usr/local/bin/grimoire"}},
				{Type: RefPath, Args: map[string]string{"path": "/var/lib/grimoire-seal/seal-keys.json"}},
				{Type: RefPath, Args: map[string]string{"path": "/var/log/syslog"}},
				{Type: RefPath, Args: map[string]string{"path": "/srv/www/app.conf"}},
			}},
		{"bare system roots are still not checked",
			"/etc /opt/ /usr/local /var/lib/ /srv", nil},
		{"a path character before the root is still inside a longer token",
			"x/etc/hosts and _/opt/y", nil},
		{"unit preceded by a slash inside a path",
			"/home/admin/bin/lectern.timer", []Anchor{{Type: RefPath, Args: map[string]string{"path": "/home/admin/bin/lectern.timer"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractRefs(c.text)
			if len(got) != len(c.want) {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
			for i := range c.want {
				if got[i].Type != c.want[i].Type || got[i].Args["path"] != c.want[i].Args["path"] || got[i].Args["unit"] != c.want[i].Args["unit"] {
					t.Errorf("ref %d = %+v, want %+v", i, got[i], c.want[i])
				}
				if err := Validate(got[i]); err != nil {
					t.Errorf("extracted ref %+v does not validate: %v", got[i], err)
				}
			}
		})
	}
}
