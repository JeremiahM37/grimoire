package filemem

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

func mkNote(path, body string) dream.Doc {
	return dream.Doc{Path: path, Body: body, Kind: dream.KindFileMemory}
}

func fm(name, desc, typ string) string {
	return fmt.Sprintf("---\nname: %s\ndescription: %s\ntype: %s\n---\n", name, desc, typ)
}

// words returns n distinct words, so two notes built from different seeds
// share no five-word shingles.
func words(seed string, n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "%sw%d ", seed, i)
	}
	return b.String()
}

func byCheck(fs []dream.Finding, check string) []dream.Finding {
	var out []dream.Finding
	for _, f := range fs {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

func TestRoots(t *testing.T) {
	tests := []struct {
		name  string
		paths []string
		want  []string
	}{
		{"none", []string{"a/b.md", "notes/x.md"}, []string{}},
		{"top and nested", []string{"MEMORY.md", "proj/MEMORY.md", "proj/x.md"}, []string{"", "proj"}},
		{"dedupes and sorts", []string{"z/MEMORY.md", "a/MEMORY.md", "z/n.md", "a/deep/MEMORY.md"}, []string{"a", "a/deep", "z"}},
		{"exact name only", []string{"memory.md", "MEMORY.md.bak", "MEMORY.mdx", "dir/memory.md"}, []string{}},
		{"cleans paths", []string{"./x/MEMORY.md"}, []string{"x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Roots(tt.paths)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Roots(%v) = %q, want %q", tt.paths, got, tt.want)
			}
		})
	}
}

func TestCheckIndexMissing(t *testing.T) {
	docs := []dream.Doc{
		mkNote("proj/MEMORY.md", "- [Linked](linked.md) — already here\n"),
		mkNote("proj/linked.md", fm("linked", "d", "user")+"body"),
		mkNote("proj/orphan.md", fm("Orphan Title", "the hook", "feedback")+"body"),
		mkNote("proj/bare.md", "no frontmatter at all"),
	}
	fs := byCheck(Check("proj", docs), "index_missing")
	if len(fs) != 2 {
		t.Fatalf("got %d index_missing findings, want 2: %+v", len(fs), fs)
	}
	tests := []struct {
		path string
		fix  dream.Fix
	}{
		// Findings follow path order, so bare.md precedes orphan.md.
		{"proj/bare.md", dream.Fix{Kind: dream.FixAppend, Path: "proj/MEMORY.md", New: "- [bare](bare.md)"}},
		{"proj/orphan.md", dream.Fix{Kind: dream.FixAppend, Path: "proj/MEMORY.md", New: "- [Orphan Title](orphan.md) — the hook"}},
	}
	for i, tt := range tests {
		f := fs[i]
		if f.Path != tt.path || f.Severity != dream.Low || f.Category != dream.Hygiene {
			t.Errorf("finding %d = %+v, want path %s, Low, Hygiene", i, f, tt.path)
		}
		if f.Fix == nil || *f.Fix != tt.fix {
			t.Errorf("finding %d fix = %+v, want %+v", i, f.Fix, tt.fix)
		}
	}
}

func TestCheckIndexMissingNoDescriptionOmitsSeparator(t *testing.T) {
	docs := []dream.Doc{
		mkNote("MEMORY.md", ""),
		mkNote("x.md", "---\nname: x\ntype: user\n---\nbody"),
	}
	fs := byCheck(Check("", docs), "index_missing")
	if len(fs) != 1 || fs[0].Fix == nil {
		t.Fatalf("got %+v", fs)
	}
	if got, want := fs[0].Fix.New, "- [x](x.md)"; got != want {
		t.Errorf("New = %q, want %q", got, want)
	}
}

func TestCheckIndexDangling(t *testing.T) {
	docs := []dream.Doc{
		mkNote("proj/MEMORY.md", "- [Gone](gone.md) — old\n- [Here](here.md) — ok\n"),
		mkNote("proj/here.md", fm("here", "d", "user")+"body"),
	}
	fs := byCheck(Check("proj", docs), "index_dangling")
	if len(fs) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Severity != dream.Medium || f.Line != 1 || f.Path != "proj/MEMORY.md" {
		t.Errorf("finding = %+v", f)
	}
	want := &dream.Fix{Kind: dream.FixReplaceLine, Path: "proj/MEMORY.md", Line: 1, Old: "- [Gone](gone.md) — old", New: ""}
	if f.Fix == nil || *f.Fix != *want {
		t.Errorf("fix = %+v, want %+v", f.Fix, want)
	}
	if !reflect.DeepEqual(f.Related, []string{"proj/gone.md"}) {
		t.Errorf("related = %v", f.Related)
	}
}

func TestCheckIndexDuplicate(t *testing.T) {
	docs := []dream.Doc{
		mkNote("MEMORY.md", "- [A](a.md) — first\n- [B](b.md)\n- [A again](a.md) — second\n- [A third](./a.md)\n"),
		mkNote("a.md", fm("a", "d", "user")+"body"),
		mkNote("b.md", fm("b", "d", "user")+"body"),
	}
	fs := byCheck(Check("", docs), "index_duplicate")
	if len(fs) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(fs), fs)
	}
	wants := []dream.Fix{
		{Kind: dream.FixReplaceLine, Path: "MEMORY.md", Line: 3, Old: "- [A again](a.md) — second", New: ""},
		{Kind: dream.FixReplaceLine, Path: "MEMORY.md", Line: 4, Old: "- [A third](./a.md)", New: ""},
	}
	for i, w := range wants {
		if fs[i].Severity != dream.Low || fs[i].Fix == nil || *fs[i].Fix != w {
			t.Errorf("finding %d = %+v, want fix %+v", i, fs[i], w)
		}
	}
}

func TestCheckIndexTruncated(t *testing.T) {
	// 201 lines: the entry on line 201 is past the line cutoff.
	var idx strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&idx, "- [n%d](n%d.md) — hook\n", i, i)
	}
	idx.WriteString("- [late](late.md) — never loaded\n")

	docs := []dream.Doc{mkNote("MEMORY.md", idx.String())}
	for i := 1; i <= 200; i++ {
		docs = append(docs, mkNote(fmt.Sprintf("n%d.md", i), fm("n", "d", "user")+"b"))
	}
	docs = append(docs, mkNote("late.md", fm("late", "d", "user")+"b"))

	fs := byCheck(Check("", docs), "index_truncated")
	if len(fs) != 1 {
		t.Fatalf("got %d truncation findings, want 1: %+v", len(fs), fs)
	}
	f := fs[0]
	if f.Severity != dream.High || f.Line != 201 || f.Fix != nil {
		t.Errorf("finding = %+v", f)
	}
	if !strings.Contains(f.Message, "1 of 201 index entries") {
		t.Errorf("message = %q", f.Message)
	}
	if !reflect.DeepEqual(f.Related, []string{"late.md"}) {
		t.Errorf("related = %v", f.Related)
	}
}

func TestCheckIndexTruncatedByBytes(t *testing.T) {
	// Under 200 lines but over 25 KiB: the byte cutoff hides the tail.
	long := strings.Repeat("x", 300)
	var idx strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&idx, "- [n%d](n%d.md) — %s\n", i, i, long)
	}
	docs := []dream.Doc{mkNote("MEMORY.md", idx.String())}
	for i := 1; i <= 100; i++ {
		docs = append(docs, mkNote(fmt.Sprintf("n%d.md", i), fm("n", "d", "user")+"b"))
	}
	fs := byCheck(Check("", docs), "index_truncated")
	if len(fs) != 1 || fs[0].Severity != dream.High {
		t.Fatalf("got %+v", fs)
	}
	if !strings.Contains(fs[0].Message, "past the 200-line / 25 KiB cutoff") {
		t.Errorf("message = %q", fs[0].Message)
	}
	if len(fs[0].Related) != maxRelated {
		t.Errorf("related has %d entries, want %d", len(fs[0].Related), maxRelated)
	}
}

func TestCheckIndexNotTruncated(t *testing.T) {
	docs := []dream.Doc{
		mkNote("MEMORY.md", "- [a](a.md) — x\n"),
		mkNote("a.md", fm("a", "d", "user")+"b"),
	}
	if fs := byCheck(Check("", docs), "index_truncated"); len(fs) != 0 {
		t.Errorf("unexpected truncation finding: %+v", fs)
	}
}

func TestCheckIndexLineLong(t *testing.T) {
	long := "- [a](a.md) — " + strings.Repeat("y", 250)
	docs := []dream.Doc{
		mkNote("MEMORY.md", "- [b](b.md) — short\n"+long+"\n"),
		mkNote("a.md", fm("a", "d", "user")+"b"),
		mkNote("b.md", fm("b", "d", "user")+"b"),
	}
	fs := byCheck(Check("", docs), "index_line_long")
	if len(fs) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(fs), fs)
	}
	if fs[0].Severity != dream.Info || fs[0].Line != 2 || fs[0].Fix != nil {
		t.Errorf("finding = %+v", fs[0])
	}
}

func TestCheckFrontmatter(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantMsg string // "" means no finding
	}{
		{"complete top-level type", fm("n", "d", "user") + "body", ""},
		{"type under metadata", "---\nname: n\ndescription: d\nmetadata:\n  type: feedback\n---\nbody", ""},
		{"folded description", "---\nname: n\ndescription: >\n  a long\n  hook\ntype: user\n---\nbody", ""},
		{"quoted values", "---\nname: \"n\"\ndescription: 'd'\ntype: user\n---\nbody", ""},
		{"no frontmatter", "just body", "note has no YAML frontmatter"},
		{"unterminated", "---\nname: n\nbody", "note has no YAML frontmatter"},
		{"missing type", "---\nname: n\ndescription: d\n---\nbody", "frontmatter is missing: type"},
		{"missing all three", "---\nfoo: bar\n---\nbody", "frontmatter is missing: name, description, type"},
		{"empty description", "---\nname: n\ndescription:\ntype: user\n---\nbody", "frontmatter is missing: description"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := byCheck(Check("", []dream.Doc{mkNote("n.md", tt.body)}), "frontmatter")
			if tt.wantMsg == "" {
				if len(fs) != 0 {
					t.Fatalf("unexpected finding: %+v", fs)
				}
				return
			}
			if len(fs) != 1 || !strings.Contains(fs[0].Message, tt.wantMsg) {
				t.Fatalf("got %+v, want message containing %q", fs, tt.wantMsg)
			}
			if fs[0].Severity != dream.Low || fs[0].Path != "n.md" {
				t.Errorf("finding = %+v", fs[0])
			}
		})
	}
}

func TestCheckNearDuplicate(t *testing.T) {
	shared := words("shared", 30)
	docs := []dream.Doc{
		mkNote("b.md", fm("b", "one", "user")+shared+"unique-b"),
		mkNote("a.md", fm("a", "two", "user")+shared+"unique-a"),
		mkNote("c.md", fm("c", "three", "user")+words("other", 30)),
	}
	fs := byCheck(Check("", docs), "near_duplicate")
	if len(fs) != 1 {
		t.Fatalf("got %d, want 1: %+v", len(fs), fs)
	}
	if fs[0].Path != "a.md" || !reflect.DeepEqual(fs[0].Related, []string{"b.md"}) {
		t.Errorf("finding = %+v; want a.md related to b.md, reported once", fs[0])
	}
	if fs[0].Severity != dream.Low {
		t.Errorf("severity = %s", fs[0].Severity)
	}
}

func TestCheckNearDuplicateSameDescription(t *testing.T) {
	docs := []dream.Doc{
		mkNote("a.md", fm("a", "Same hook", "user")+words("aa", 25)),
		mkNote("b.md", fm("b", "same hook", "user")+words("bb", 25)),
	}
	fs := byCheck(Check("", docs), "near_duplicate")
	if len(fs) != 1 || !strings.Contains(fs[0].Message, "same description") {
		t.Fatalf("got %+v", fs)
	}
}

func TestCheckNearDuplicateSkipsShortBodies(t *testing.T) {
	// Identical text, but under 20 words each: skipped, per the pair rule.
	short := "one two three four five six seven eight nine ten"
	docs := []dream.Doc{
		mkNote("a.md", fm("a", "same", "user")+short),
		mkNote("b.md", fm("b", "same", "user")+short),
	}
	if fs := byCheck(Check("", docs), "near_duplicate"); len(fs) != 0 {
		t.Errorf("short notes flagged: %+v", fs)
	}
}

func TestCheckNearDuplicateBelowThreshold(t *testing.T) {
	// Half the shingles shared: Jaccard well under 0.6.
	docs := []dream.Doc{
		mkNote("a.md", fm("a", "d1", "user")+words("aa", 40)),
		mkNote("b.md", fm("b", "d2", "user")+words("aa", 20)+words("bb", 20)),
	}
	if fs := byCheck(Check("", docs), "near_duplicate"); len(fs) != 0 {
		t.Errorf("below-threshold pair flagged: %+v", fs)
	}
}

func TestCheckOnlyDirectChildrenAndNoIndex(t *testing.T) {
	docs := []dream.Doc{
		mkNote("proj/a.md", "no frontmatter"),
		mkNote("proj/sub/b.md", "no frontmatter"),
		mkNote("other/c.md", "no frontmatter"),
	}
	fs := Check("proj", docs)
	if len(fs) != 1 || fs[0].Path != "proj/a.md" {
		t.Fatalf("got %+v; want only proj/a.md", fs)
	}
	// No MEMORY.md in root: index checks do not run, so nothing is "unlinked".
	if n := len(byCheck(fs, "index_missing")); n != 0 {
		t.Errorf("index_missing reported without an index")
	}
}

func TestCheckCleanStoreHasNoFindings(t *testing.T) {
	docs := []dream.Doc{
		mkNote("MEMORY.md", "- [a](a.md) — hook a\n- [b](b.md) — hook b\n"),
		mkNote("a.md", fm("a", "hook a", "user")+words("aa", 30)),
		mkNote("b.md", fm("b", "hook b", "feedback")+words("bb", 30)),
	}
	if fs := Check("", docs); len(fs) != 0 {
		t.Errorf("clean store produced findings: %+v", fs)
	}
}

func TestFragmented(t *testing.T) {
	base := t.TempDir()
	projects := filepath.Join(base, "projects")
	canonical := filepath.Join(projects, "-home-admin", "memory")

	mkdir := func(p string) {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		t.Helper()
		mkdir(filepath.Dir(p))
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// The canonical store, with notes: must never be reported.
	write(filepath.Join(canonical, "MEMORY.md"), "- [x](x.md)\n")
	write(filepath.Join(canonical, "x.md"), "note")

	// A real directory with a note: fragmented.
	frag := filepath.Join(projects, "-home-admin-projects-foo", "memory")
	write(filepath.Join(frag, "feedback_a.md"), "note")
	write(filepath.Join(frag, "notes.txt"), "not markdown")

	// A real directory with only an empty index: not fragmented.
	write(filepath.Join(projects, "-empty", "memory", "MEMORY.md"), "  \n")

	// A real directory whose index has content: fragmented.
	write(filepath.Join(projects, "-index-only", "memory", "MEMORY.md"), "- [a](a.md)\n")

	// A symlinked memory directory pointing at a populated store: ignored.
	target := filepath.Join(base, "elsewhere")
	write(filepath.Join(target, "leak.md"), "note")
	linkParent := filepath.Join(projects, "-linked")
	mkdir(linkParent)
	if err := os.Symlink(target, filepath.Join(linkParent, "memory")); err != nil {
		t.Fatal(err)
	}

	// A symlink to the canonical store itself: also ignored.
	canonLink := filepath.Join(projects, "-canon-link")
	mkdir(canonLink)
	if err := os.Symlink(canonical, filepath.Join(canonLink, "memory")); err != nil {
		t.Fatal(err)
	}

	fs := Fragmented(projects, canonical)
	if len(fs) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(fs), fs)
	}
	wantFrag := []struct {
		path    string
		related []string
	}{
		{filepath.Join(frag), []string{"feedback_a.md"}},
		{filepath.Join(projects, "-index-only", "memory"), []string{"MEMORY.md"}},
	}
	for i, w := range wantFrag {
		f := fs[i]
		if f.Check != "fragmented_store" || f.Category != dream.Hygiene || f.Severity != dream.Medium {
			t.Errorf("finding %d = %+v", i, f)
		}
		if f.Path != w.path {
			t.Errorf("finding %d path = %s, want %s", i, f.Path, w.path)
		}
		if !reflect.DeepEqual(f.Related, w.related) {
			t.Errorf("finding %d related = %v, want %v", i, f.Related, w.related)
		}
	}
}

func TestFragmentedMissingDir(t *testing.T) {
	if fs := Fragmented(filepath.Join(t.TempDir(), "does-not-exist"), "/x/memory"); fs != nil {
		t.Errorf("got %+v, want nil", fs)
	}
}
