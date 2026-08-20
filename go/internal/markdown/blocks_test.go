package markdown

import "testing"

func kinds(blocks []Block) []string {
	out := make([]string, len(blocks))
	for i, b := range blocks {
		out[i] = b.Kind + ":" + b.Text
	}
	return out
}

func TestParseBlocksFindsHeadingsItemsAndTasks(t *testing.T) {
	body := "# Rollout\n\nsome prose\n\n" +
		"- first bullet\n" +
		"- [ ] an open task\n" +
		"- [x] a done task\n" +
		"\n## Risks\n\n" +
		"* a starred bullet\n" +
		"+ a plussed bullet\n"
	got := ParseBlocks(body)
	want := []string{
		"heading:Rollout", "item:first bullet", "task:an open task",
		"task:a done task", "heading:Risks", "item:a starred bullet",
		"item:a plussed bullet",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", kinds(got), want)
	}
	for i := range got {
		if kinds(got)[i] != want[i] {
			t.Errorf("block %d = %q, want %q", i, kinds(got)[i], want[i])
		}
	}
}

func TestParseBlocksRecordsLevelsAndLines(t *testing.T) {
	body := "# One\n## Two\n### Three\n- top\n  - nested\n    - deeper\n"
	got := ParseBlocks(body)
	levels := map[string]int{}
	lines := map[string]int{}
	for _, b := range got {
		levels[b.Text] = b.Level
		lines[b.Text] = b.Line
	}
	for text, want := range map[string]int{
		"One": 1, "Two": 2, "Three": 3, "top": 1, "nested": 2, "deeper": 3} {
		if levels[text] != want {
			t.Errorf("%q level = %d, want %d", text, levels[text], want)
		}
	}
	if lines["nested"] != 4 {
		t.Errorf("nested is on line %d, want 4", lines["nested"])
	}
}

func TestTabsAndSpacesNestTheSame(t *testing.T) {
	// Editors disagree about which they emit and a note often has both;
	// "top-level" has to mean what it looks like on screen.
	spaces := ParseBlocks("- a\n  - b\n")
	tabs := ParseBlocks("- a\n\t- b\n")
	if spaces[1].Level != tabs[1].Level {
		t.Errorf("space-nested level %d, tab-nested level %d",
			spaces[1].Level, tabs[1].Level)
	}
}

func TestTaskCheckedState(t *testing.T) {
	got := ParseBlocks("- [ ] open\n- [x] done\n- [X] also done\n")
	if len(got) != 3 {
		t.Fatalf("got %v", kinds(got))
	}
	for i, want := range []bool{false, true, true} {
		if got[i].Checked != want {
			t.Errorf("%q checked = %v, want %v", got[i].Text, got[i].Checked, want)
		}
	}
}

func TestBlocksRememberTheirSection(t *testing.T) {
	// "which section is this bullet in" without re-reading the note.
	body := "# Rollout\n- prep the box\n\n## Risks\n- [ ] the disk might fill\n"
	got := ParseBlocks(body)
	var task Block
	for _, b := range got {
		if b.Kind == KindTask {
			task = b
		}
	}
	if task.Parent != "Risks" {
		t.Errorf("task's section = %q, want Risks", task.Parent)
	}
	if got[1].Parent != "Rollout" {
		t.Errorf("item's section = %q, want Rollout", got[1].Parent)
	}
}

func TestCodeFencesAreNotBlocks(t *testing.T) {
	// A shell script would otherwise fill the index with headings nobody
	// wrote, and a YAML block with a list item per key.
	body := "# Real Heading\n\n```bash\n# not a heading\n- not an item\n```\n\n" +
		"~~~yaml\n- also not an item\n~~~\n\n- a real item\n"
	got := ParseBlocks(body)
	if len(got) != 2 {
		t.Fatalf("fenced lines leaked in: %v", kinds(got))
	}
	if got[0].Text != "Real Heading" || got[1].Text != "a real item" {
		t.Errorf("got %v", kinds(got))
	}
}

func TestEmptyBulletIsNotABlock(t *testing.T) {
	if got := ParseBlocks("- \n-\n- real\n"); len(got) != 1 {
		t.Errorf("an empty bullet became a block: %v", kinds(got))
	}
}

func TestClosingHashesAreNotPartOfAHeading(t *testing.T) {
	got := ParseBlocks("## Setup ##\n")
	if len(got) != 1 || got[0].Text != "Setup" {
		t.Errorf("got %v", kinds(got))
	}
}

func TestSevenHashesIsNotAHeading(t *testing.T) {
	if got := ParseBlocks("####### too deep\n"); len(got) != 0 {
		t.Errorf("got %v", kinds(got))
	}
}

func TestParseBlocksOnEmptyBody(t *testing.T) {
	if got := ParseBlocks(""); len(got) != 0 {
		t.Errorf("got %v", kinds(got))
	}
}
