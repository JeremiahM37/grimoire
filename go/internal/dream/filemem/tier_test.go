package filemem

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
)

func tierDocs(n int) []dream.Doc {
	var idx []string
	var docs []dream.Doc
	for i := 0; i < n; i++ {
		kind := "project"
		if i%5 == 0 {
			kind = "feedback"
		}
		file := fmt.Sprintf("%s_note%03d.md", kind, i)
		idx = append(idx, fmt.Sprintf("- [Note %d](%s) — %s", i, file, strings.Repeat("hook words ", 12)))
		docs = append(docs, dream.Doc{Path: "mem/" + file, Kind: dream.KindFileMemory,
			Body:  "---\nname: n\ndescription: d\nmetadata:\n  type: " + kind + "\n---\nbody\n",
			Mtime: time.Unix(int64(i), 0)})
	}
	docs = append(docs, dream.Doc{Path: "mem/MEMORY.md", Kind: dream.KindFileMemory, Body: strings.Join(idx, "\n") + "\n"})
	return docs
}

func applyAll(t *testing.T, docs []dream.Doc, fixes []dream.Fix) map[string]string {
	t.Helper()
	files := map[string]string{}
	for _, d := range docs {
		files[d.Path] = d.Body
	}
	for _, f := range fixes {
		if files[f.Path] != f.Old {
			t.Fatalf("fix for %s planned against stale text", f.Path)
		}
		if f.Kind == dream.FixRemoveFile {
			delete(files, f.Path)
			continue
		}
		files[f.Path] = f.New
	}
	return files
}

func TestTierKeepsEveryEntryAndFitsTheLimit(t *testing.T) {
	docs := tierDocs(240)
	fixes := Tier("mem", docs, map[string]int{"project_note001.md": 50})
	if len(fixes) == 0 {
		t.Fatal("an over-limit index got no plan")
	}
	files := applyAll(t, docs, fixes)
	core := files["mem/MEMORY.md"]
	if OverLimit(core) {
		t.Fatalf("core still over the limit: %d bytes", len(core))
	}
	seen := map[string]int{}
	for p, body := range files {
		if p != "mem/MEMORY.md" && !strings.HasPrefix(p, "mem/MEMORY-") {
			continue
		}
		for _, l := range strings.Split(body, "\n") {
			if tgt := linkTarget(l); tgt != "" && !IsIndexPart(tgt) {
				seen[tgt]++
			}
		}
	}
	if len(seen) != 240 {
		t.Fatalf("entries across index files = %d, want 240", len(seen))
	}
	for tgt, n := range seen {
		if n != 1 {
			t.Fatalf("%s listed %d times", tgt, n)
		}
	}
	for i := 0; i < 240; i += 5 {
		if !strings.Contains(core, fmt.Sprintf("feedback_note%03d.md", i)) {
			t.Fatalf("rule feedback_note%03d left the core", i)
		}
	}
	if !strings.Contains(core, "project_note001.md") {
		t.Fatal("the most-used project note left the core")
	}
	if !strings.Contains(core, "(MEMORY-project.md)") {
		t.Fatal("core does not point at the project part")
	}

	// A second plan over the result is empty: tiering is idempotent.
	var again []dream.Doc
	for p, body := range files {
		again = append(again, dream.Doc{Path: p, Body: body, Kind: dream.KindFileMemory})
	}
	if extra := Tier("mem", again, nil); len(extra) != 0 {
		t.Fatalf("tiering a tiered index changed %d files", len(extra))
	}
	// And the index checks see parts as part of the index: nothing is
	// reported missing, nothing truncated.
	for _, f := range Check("mem", again) {
		if f.Check == "index_missing" || f.Check == "index_truncated" || f.Check == "index_dangling" {
			t.Fatalf("unexpected %s on %s", f.Check, f.Path)
		}
	}
}

func TestTierLeavesAnIndexUnderTheLimitAlone(t *testing.T) {
	if fixes := Tier("mem", tierDocs(20), nil); fixes != nil {
		t.Fatalf("small index got %d fixes", len(fixes))
	}
}

func TestFrontmatterFixesFromTheFileName(t *testing.T) {
	docs := []dream.Doc{
		{Path: "mem/MEMORY.md", Kind: dream.KindFileMemory, Body: "- [a](project_a.md)\n- [b](reference_b.md)\n- [c](notes_c.md)\n"},
		{Path: "mem/project_a.md", Kind: dream.KindFileMemory, Body: "# Alpha rollout\n\ntext\n"},
		{Path: "mem/reference_b.md", Kind: dream.KindFileMemory, Body: "---\ndescription: x\n---\nbody\n"},
		{Path: "mem/notes_c.md", Kind: dream.KindFileMemory, Body: "# C\n"},
	}
	fixes := map[string]*dream.Fix{}
	for _, f := range Check("mem", docs) {
		if f.Check == "frontmatter" {
			fixes[f.Path] = f.Fix
		}
	}
	a := fixes["mem/project_a.md"]
	if a == nil || !strings.Contains(a.New, "type: project") || !strings.Contains(a.New, `description: "Alpha rollout"`) || !strings.HasSuffix(a.New, "\n# Alpha rollout") {
		t.Fatalf("project_a fix = %+v", a)
	}
	b := fixes["mem/reference_b.md"]
	if b == nil || b.New != "---\nname: reference-b\ntype: reference" {
		t.Fatalf("reference_b fix = %+v", b)
	}
	if fixes["mem/notes_c.md"] != nil {
		t.Fatal("a type the file name does not declare must not be guessed")
	}
	// A procedure Grimoire wrote declares itself under metadata.kind.
	proc := []dream.Doc{{Path: "mem/procedure_x.md", Kind: dream.KindFileMemory,
		Body: "---\nname: x\ndescription: y\nmetadata:\n  kind: procedure\n---\nsteps\n"}}
	for _, f := range Check("mem", proc) {
		if f.Check == "frontmatter" {
			t.Fatalf("procedure flagged: %s", f.Message)
		}
	}
}
