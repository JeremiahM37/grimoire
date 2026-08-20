package index

import (
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
)

func blockTexts(blocks []Block) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Text
	}
	return out
}

func rolloutVault(t *testing.T) *Index {
	t.Helper()
	ix := testIndex(t)
	write(t, ix, "plan.md", "# Rollout\n\n- prep the box\n- [ ] drain the queue\n"+
		"- [x] take a backup\n\n## Risks\n\n- [ ] the disk might fill\n  - nested detail\n")
	write(t, ix, "other/notes.md", "# Other\n\n- [ ] an unrelated task\n")
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	return ix
}

func TestBlocksAreIndexed(t *testing.T) {
	ix := rolloutVault(t)
	got, err := ix.Blocks(BlockQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 9 {
		t.Fatalf("got %d blocks: %v", len(got), blockTexts(got))
	}
	// Ordered by note path then line, so each note's blocks read like the
	// file they came from. "other/notes.md" sorts before "plan.md".
	if got[0].Note != "other/notes.md" || got[0].Kind != markdown.KindHeading {
		t.Errorf("first block = %+v", got[0])
	}
	if got[0].Title != "Other" {
		t.Errorf("block does not carry its note's title: %+v", got[0])
	}
	if got[1].Note != "other/notes.md" || got[1].Line <= got[0].Line {
		t.Errorf("blocks within a note are out of order: %+v", got[1])
	}
}

func TestBlocksFilterByKindAndState(t *testing.T) {
	ix := rolloutVault(t)
	done := true
	open := false
	cases := []struct {
		name string
		q    BlockQuery
		want []string
	}{
		{"headings", BlockQuery{Kind: markdown.KindHeading},
			[]string{"Other", "Rollout", "Risks"}},
		{"open tasks", BlockQuery{Kind: markdown.KindTask, Checked: &open},
			[]string{"an unrelated task", "drain the queue", "the disk might fill"}},
		{"done tasks", BlockQuery{Kind: markdown.KindTask, Checked: &done},
			[]string{"take a backup"}},
		{"items", BlockQuery{Kind: markdown.KindItem},
			[]string{"prep the box", "nested detail"}},
		{"one note", BlockQuery{Note: "other/notes.md"},
			[]string{"Other", "an unrelated task"}},
		{"a path prefix", BlockQuery{Path: "other/", Kind: markdown.KindTask},
			[]string{"an unrelated task"}},
		{"a section", BlockQuery{Section: "Risks"},
			[]string{"the disk might fill", "nested detail"}},
		{"a nesting level", BlockQuery{Kind: markdown.KindItem, Level: 2},
			[]string{"nested detail"}},
		{"text", BlockQuery{Text: "DISK"}, []string{"the disk might fill"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ix.Blocks(c.q)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("got %v, want %v", blockTexts(got), c.want)
			}
			for i := range got {
				if got[i].Text != c.want[i] {
					t.Fatalf("got %v, want %v", blockTexts(got), c.want)
				}
			}
		})
	}
}

func TestBlockTextSearchIsLiteral(t *testing.T) {
	// A search for "50%" must match "50%", not everything.
	ix := testIndex(t)
	write(t, ix, "a.md", "- the disk is 50% full\n- an unrelated bullet\n")
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	got, _ := ix.Blocks(BlockQuery{Text: "50%"})
	if len(got) != 1 {
		t.Fatalf("got %v", blockTexts(got))
	}
}

func TestBlocksRebuildOnEditAndVanishWithTheNote(t *testing.T) {
	ix := rolloutVault(t)
	write(t, ix, "plan.md", "# Rollout\n\n- only one bullet now\n")
	if _, err := ix.Upsert("plan.md"); err != nil {
		t.Fatal(err)
	}
	got, _ := ix.Blocks(BlockQuery{Note: "plan.md"})
	if len(got) != 2 {
		t.Fatalf("stale rows survived an edit: %v", blockTexts(got))
	}
	if err := ix.Remove("plan.md"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ix.Blocks(BlockQuery{Note: "plan.md"}); len(got) != 0 {
		t.Fatalf("rows outlived their note: %v", blockTexts(got))
	}
}

func TestBlocksRespectPrivacy(t *testing.T) {
	ix := testIndex(t)
	fm := markdown.NewFrontmatter()
	fm.Set("private", true)
	if _, err := ix.Vault.Write("secret.md", "# Secret\n\n- [ ] a private task\n", fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Upsert("secret.md"); err != nil {
		t.Fatal(err)
	}
	if got, _ := ix.Blocks(BlockQuery{}); len(got) != 0 {
		t.Fatalf("a private note's lines leaked: %v", blockTexts(got))
	}
	got, _ := ix.Blocks(BlockQuery{Filter: Filter{IncludePrivate: true}})
	if len(got) != 2 {
		t.Fatalf("opting in returned %v", blockTexts(got))
	}
}

func TestBlocksLimit(t *testing.T) {
	ix := rolloutVault(t)
	got, _ := ix.Blocks(BlockQuery{Limit: 2})
	if len(got) != 2 {
		t.Fatalf("limit ignored: %d", len(got))
	}
}
