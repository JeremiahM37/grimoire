package skillexport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memstore"
)

func note(file, raw string) memstore.Note {
	return memstore.ParseNote(file, raw, time.Unix(1790000000, 0))
}

const deploy = "---\nname: Deploy the kestrel service\ndescription: How to ship kestrel\nmetadata:\n  kind: procedure\n  cues: deploying kestrel; releasing a new kestrel build\n---\n\n# Deploy the kestrel service\n\n1. Build:\n```bash\nmake build\n```\n2. Ship:\n```sh\nscp out/kestrel host:/srv/\n```\n3. Check the logs.\n"

const plain = "---\nname: Rotate logs\ndescription: how to rotate logs\nmetadata:\n  kind: procedure\n---\n\n1. stop it\n2. rotate\n3. start it\n"

const rule = "---\nname: Never force push\ndescription: x\nmetadata:\n  kind: rule\n---\nNever.\n"

func fixed() func() time.Time {
	t := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

func TestRenderBuildsNameDescriptionFromCuesAndScripts(t *testing.T) {
	sk, err := Render(note("reference_deploy.md", deploy))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Name != "deploy-the-kestrel-service" {
		t.Errorf("name %q", sk.Name)
	}
	if !strings.Contains(sk.Description, "Use when deploying kestrel; or when releasing a new kestrel build") {
		t.Errorf("description %q", sk.Description)
	}
	md := sk.Files["SKILL.md"]
	if !strings.HasPrefix(md, "---\nname: deploy-the-kestrel-service\ndescription: ") || !strings.Contains(md, "1. Build:") {
		t.Errorf("SKILL.md:\n%s", md)
	}
	if strings.Count(md, "# Deploy the kestrel service") != 1 {
		t.Error("the body's own H1 should not be repeated")
	}
	sh := sk.Files["scripts/steps.sh"]
	if !strings.Contains(sh, "make build") || !strings.Contains(sh, "# step 2") || !strings.Contains(sh, "scp out/kestrel") {
		t.Errorf("scripts/steps.sh:\n%s", sh)
	}
	plainSk, _ := Render(note("p.md", plain))
	if _, has := plainSk.Files["scripts/steps.sh"]; has || len(plainSk.Files) != 1 {
		t.Errorf("a procedure with no shell blocks needs no scripts/: %v", plainSk.Files)
	}
	if !strings.Contains(plainSk.Description, "how to rotate logs.") {
		t.Errorf("description falls back to the note's: %q", plainSk.Description)
	}
}

func TestExportIsIdempotentAndNeverTouchesForeignSkills(t *testing.T) {
	dir := t.TempDir()
	notes := []memstore.Note{note("reference_deploy.md", deploy), note("p.md", plain), note("r.md", rule)}
	// A skill the user wrote by hand under a name we would use.
	if err := os.MkdirAll(filepath.Join(dir, "rotate-logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	mine := filepath.Join(dir, "rotate-logs", "SKILL.md")
	if err := os.WriteFile(mine, []byte("my own skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	opt := Options{Agent: "claude-code", Dir: dir, Now: fixed()}

	res, err := Export(notes, opt)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range res {
		got[r.Name] = r.Action
	}
	if got["deploy-the-kestrel-service"] != "created" || got["rotate-logs"] != "skipped" || len(res) != 2 {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(mine); string(b) != "my own skill\n" {
		t.Fatal("a skill we did not write was modified")
	}
	if _, err := os.Stat(filepath.Join(dir, "deploy-the-kestrel-service", "scripts", "steps.sh")); err != nil {
		t.Fatal(err)
	}
	mk, ok := readMarker(filepath.Join(dir, "deploy-the-kestrel-service"))
	if !ok || mk.Agent != "claude-code" || mk.Source != "reference_deploy.md" || mk.ExportedAt.IsZero() || len(mk.Files) != 2 {
		t.Fatalf("marker %+v", mk)
	}

	// Second run: nothing changes, not even the export date.
	later := opt
	later.Now = func() time.Time { return fixed()().Add(48 * time.Hour) }
	res, _ = Export(notes, later)
	for _, r := range res {
		if r.Name == "deploy-the-kestrel-service" && r.Action != "unchanged" {
			t.Errorf("second run: %+v", r)
		}
	}
	mk2, _ := readMarker(filepath.Join(dir, "deploy-the-kestrel-service"))
	if !mk2.ExportedAt.Equal(mk.ExportedAt) || !mk2.UpdatedAt.IsZero() {
		t.Errorf("marker moved on a no-op: %+v", mk2)
	}

	// The memory changes: updated, export date kept, update date set.
	changed := strings.Replace(deploy, "3. Check the logs.", "3. Check the logs and the dashboard.", 1)
	res, _ = Export([]memstore.Note{note("reference_deploy.md", changed)}, later)
	if res[0].Action != "updated" {
		t.Fatalf("%+v", res)
	}
	mk3, _ := readMarker(filepath.Join(dir, "deploy-the-kestrel-service"))
	if !mk3.ExportedAt.Equal(mk.ExportedAt) || mk3.UpdatedAt.IsZero() {
		t.Errorf("marker %+v", mk3)
	}

	// Hand edits to a managed skill are respected unless forced.
	skill := filepath.Join(dir, "deploy-the-kestrel-service", "SKILL.md")
	if err := os.WriteFile(skill, []byte("hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = Export([]memstore.Note{note("reference_deploy.md", deploy)}, later)
	if res[0].Action != "skipped" || !strings.Contains(res[0].Detail, "edited by hand") {
		t.Fatalf("%+v", res)
	}
	if b, _ := os.ReadFile(skill); string(b) != "hand edited\n" {
		t.Fatal("hand edit was overwritten")
	}
	later.Force = true
	if res, _ = Export([]memstore.Note{note("reference_deploy.md", deploy)}, later); res[0].Action != "updated" {
		t.Fatalf("%+v", res)
	}
}

func TestDryRunWritesNothingAndSecretsAreNotExported(t *testing.T) {
	dir := t.TempDir()
	leaky := "---\nname: Use the token\nmetadata:\n  kind: procedure\n---\n1. export TOKEN=ghp_" + strings.Repeat("aB3dE5gH7j", 4) + "\n2. run\n3. done\n"
	res, _ := Export([]memstore.Note{note("a.md", plain), note("b.md", leaky)}, Options{Agent: "x", Dir: dir, DryRun: true, Now: fixed()})
	if len(res) != 2 || res[0].Action != "created" || res[1].Action != "skipped" {
		t.Fatalf("%+v", res)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("dry run wrote %v", entries)
	}
	if _, err := Export(nil, Options{Agent: "x"}); err == nil {
		t.Error("no skills dir must be an error")
	}
}

func TestRemoveDeletesOnlyWhatItWrote(t *testing.T) {
	dir := t.TempDir()
	opt := Options{Agent: "codex", Dir: dir, Now: fixed()}
	if _, err := Export([]memstore.Note{note("reference_deploy.md", deploy), note("p.md", plain)}, opt); err != nil {
		t.Fatal(err)
	}
	// A hand-made skill, and a stray file the user added inside a managed one.
	os.MkdirAll(filepath.Join(dir, "handmade"), 0o755)
	os.WriteFile(filepath.Join(dir, "handmade", "SKILL.md"), []byte("keep\n"), 0o644)
	stray := filepath.Join(dir, "rotate-logs", "notes.txt")
	os.WriteFile(stray, []byte("mine\n"), 0o644)
	// A skill managed for a different agent in the same dir.
	other := opt
	other.Agent = "claude-code"
	Export([]memstore.Note{note("x.md", "---\nname: Other thing\nmetadata:\n  kind: procedure\n---\n1. a\n2. b\n3. c\n")}, other)

	res, err := Remove(Options{Agent: "codex", Dir: dir, DryRun: true})
	if err != nil || len(res) != 2 {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(dir, "rotate-logs", "SKILL.md")); err != nil {
		t.Fatal("dry run removed something")
	}
	if _, err := Remove(Options{Agent: "codex", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"deploy-the-kestrel-service", "handmade/SKILL.md", "rotate-logs/notes.txt", "other-thing/SKILL.md"} {
		_, err := os.Stat(filepath.Join(dir, p))
		want := p != "deploy-the-kestrel-service"
		if (err == nil) != want {
			t.Errorf("%s exists=%v, want %v", p, err == nil, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "rotate-logs", "SKILL.md")); err == nil {
		t.Error("the skill file we wrote should be gone")
	}
	if _, err := os.Stat(filepath.Join(dir, "rotate-logs", MarkerFile)); err == nil {
		t.Error("marker should be gone")
	}
}

func TestOrphansAreReportedNotDeleted(t *testing.T) {
	dir := t.TempDir()
	opt := Options{Agent: "codex", Dir: dir, Now: fixed()}
	Export([]memstore.Note{note("p.md", plain)}, opt)
	res, _ := Export(nil, opt)
	if len(res) != 1 || res[0].Action != "orphan" {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "rotate-logs", "SKILL.md")); err != nil {
		t.Error("an orphan must stay until --remove")
	}
}
