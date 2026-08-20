package queries_test

import (
	"path/filepath"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/queries"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// A query block is user-authored text that becomes SQL, so these tests are as
// much about what it CANNOT do as about what it returns.

func testDB(t *testing.T) *db.DB {
	t.Helper()
	root := t.TempDir()
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ix := index.New(database, v, embed.Hash{})

	write := func(rel, body string, fm map[string]any) {
		f := markdown.NewFrontmatter()
		for k, val := range fm {
			switch typed := val.(type) {
			case string:
				f.Set(k, typed)
			case bool:
				f.Set(k, typed)
			}
		}
		if _, err := v.Write(rel, body, f); err != nil {
			t.Fatal(err)
		}
	}
	write("plan.md", "# Rollout\n\n- prep the box\n- [ ] drain the queue\n"+
		"- [x] take a backup\n\n## Risks\n\n- [ ] the disk might fill\n"+
		"  - nested detail\n\nlinks to [[Other]] #infra\n", map[string]any{"title": "Rollout"})
	write("other.md", "# Other\n\n- [ ] an unrelated task\n", map[string]any{"title": "Other"})
	write("secret.md", "# Secret\n\n- [ ] a private task\n",
		map[string]any{"title": "Secret", "private": true})
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	return database
}

func run(t *testing.T, database *db.DB, block string) *queries.Result {
	t.Helper()
	res := queries.Run(database, block, false)
	if len(res.Errors) > 0 {
		t.Fatalf("query %q errored: %v", block, res.Errors)
	}
	return res
}

func field(rows []map[string]any, key string) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r[key].(string)
	}
	return out
}

func TestNotesRemainTheDefaultSource(t *testing.T) {
	database := testDB(t)
	res := run(t, database, "tag: infra")
	if len(res.Rows) != 1 || res.Rows[0]["path"] != "plan.md" {
		t.Fatalf("got %v", field(res.Rows, "path"))
	}
}

func TestQueryOpenTasks(t *testing.T) {
	database := testDB(t)
	res := run(t, database, "from: tasks\nchecked: false\nsort: note asc")
	got := field(res.Rows, "text")
	want := []string{"an unrelated task", "drain the queue", "the disk might fill"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestQueryDoneTasksAndHeadings(t *testing.T) {
	database := testDB(t)
	if got := field(run(t, database, "from: tasks\nchecked: true").Rows, "text"); len(got) != 1 ||
		got[0] != "take a backup" {
		t.Errorf("done tasks = %v", got)
	}
	if got := field(run(t, database, "from: headings").Rows, "text"); len(got) != 3 {
		t.Errorf("headings = %v", got)
	}
	if got := field(run(t, database, "from: headings\nlevel: 2").Rows, "text"); len(got) != 1 ||
		got[0] != "Risks" {
		t.Errorf("level 2 headings = %v", got)
	}
}

func TestQueryBlocksBySectionPathAndText(t *testing.T) {
	database := testDB(t)
	for block, want := range map[string]string{
		"from: items\nsection: Risks":                   "nested detail",
		"from: tasks\npath: other":                      "an unrelated task",
		"from: tasks\ntext: DISK":                       "the disk might fill",
		"from: items\nlevel: 2":                         "nested detail",
		"from: tasks\ntag: infra\nsort: line asc":       "drain the queue",
		"from: tasks\nlinked_to: Other\nsort: line asc": "drain the queue",
	} {
		got := field(run(t, database, block).Rows, "text")
		if len(got) == 0 || got[0] != want {
			t.Errorf("%q returned %v, want %q first", block, got, want)
		}
	}
}

func TestBlockRowsCarryTheirNotePath(t *testing.T) {
	// Not decoration: the HTTP layer filters query results by `path` against
	// what the caller may read, so a block row without one would bypass the
	// check that every note row goes through.
	database := testDB(t)
	for _, row := range run(t, database, "from: tasks").Rows {
		if row["path"] == "" || row["path"] != row["note"] {
			t.Fatalf("row has no usable path: %v", row)
		}
	}
}

func TestPrivateNotesLinesAreExcluded(t *testing.T) {
	database := testDB(t)
	for _, text := range field(run(t, database, "from: tasks").Rows, "text") {
		if text == "a private task" {
			t.Fatal("a private note's line leaked into a query block")
		}
	}
	res := queries.Run(database, "from: tasks", true)
	var found bool
	for _, text := range field(res.Rows, "text") {
		if text == "a private task" {
			found = true
		}
	}
	if !found {
		t.Error("opting in did not include it")
	}
}

func TestBlockDefaultsSuitLines(t *testing.T) {
	// A line has no title and no updated date of its own, so the note-shaped
	// defaults would render a table of blanks.
	database := testDB(t)
	res := run(t, database, "from: tasks")
	if len(res.Columns) == 0 || res.Columns[0] != "text" {
		t.Errorf("default columns = %v", res.Columns)
	}
}

func TestBlockFiltersAreRejectedAgainstNotes(t *testing.T) {
	// Silently ignoring a filter shows the wrong rows and looks like the query
	// working, which is the worst of the three options.
	database := testDB(t)
	for _, block := range []string{"checked: false", "section: Risks", "level: 2"} {
		if res := queries.Run(database, block, false); len(res.Errors) == 0 {
			t.Errorf("%q was accepted against notes", block)
		}
	}
	if res := queries.Run(database, "from: items\nchecked: false", false); len(res.Errors) == 0 {
		t.Error("'checked' was accepted against items")
	}
}

func TestUnknownSourceAndColumnsAreErrors(t *testing.T) {
	database := testDB(t)
	for _, block := range []string{
		"from: everything",
		"from: tasks\ncolumns: updated",
		"from: tasks\nsort: updated",
		"from: tasks\nlevel: nope",
	} {
		if res := queries.Run(database, block, false); len(res.Errors) == 0 {
			t.Errorf("%q was accepted", block)
		}
	}
}

func TestBlockTextSearchCannotInject(t *testing.T) {
	// The whole design rule of this package: user text is a bound parameter
	// and its metacharacters are neutralized.
	database := testDB(t)
	for _, needle := range []string{"%", "_", `\`, "' OR 1=1 --"} {
		res := queries.Run(database, "from: tasks\ntext: "+needle, false)
		if len(res.Errors) > 0 {
			t.Fatalf("text %q errored: %v", needle, res.Errors)
		}
		if len(res.Rows) > 0 {
			t.Errorf("text %q matched %d rows, want none", needle, len(res.Rows))
		}
	}
}

func TestBlockQueryIsCapped(t *testing.T) {
	database := testDB(t)
	res := run(t, database, "from: tasks\nlimit: 1")
	if len(res.Rows) != 1 {
		t.Errorf("limit ignored: %d rows", len(res.Rows))
	}
}
