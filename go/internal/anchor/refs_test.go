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
