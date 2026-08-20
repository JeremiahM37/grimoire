package queries_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/queries"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The database-view half: filtering, showing and grouping by the fields people
// keep in frontmatter.

func projectDB(t *testing.T) *db.DB {
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

	notes := []struct {
		path  string
		props map[string]string
	}{
		{"alpha.md", map[string]string{"title": "Alpha", "status": "active",
			"priority": "3", "owner": "Ana Diaz", "due": "2026-09-01"}},
		{"beta.md", map[string]string{"title": "Beta", "status": "done",
			"priority": "10", "owner": "Bo Chen", "due": "2026-08-01"}},
		{"gamma.md", map[string]string{"title": "Gamma", "status": "active",
			"priority": "9"}},
		{"delta.md", map[string]string{"title": "Delta"}},
	}
	for _, n := range notes {
		fm := markdown.NewFrontmatter()
		for k, val := range n.props {
			fm.Set(k, val)
		}
		if _, err := v.Write(n.path, "# "+n.props["title"]+"\n\nbody\n", fm); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	return database
}

func titles(rows []map[string]any) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i], _ = r["title"].(string)
	}
	return out
}

func has(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestFilterByProperty(t *testing.T) {
	database := projectDB(t)
	cases := map[string][]string{
		"where: status = active":                      {"Alpha", "Gamma"},
		"where: status != active":                     {"Beta", "Delta"},
		"where: status exists":                        {"Alpha", "Beta", "Gamma"},
		"where: status missing":                       {"Delta"},
		"where: owner contains ana":                   {"Alpha"},
		"where: priority > 5":                         {"Beta", "Gamma"},
		"where: priority >= 9":                        {"Beta", "Gamma"},
		"where: priority < 5":                         {"Alpha"},
		"where: status = active\nwhere: priority > 5": {"Gamma"},
	}
	for block, want := range cases {
		res := queries.Run(database, block, false)
		if len(res.Errors) > 0 {
			t.Errorf("%q errored: %v", block, res.Errors)
			continue
		}
		got := titles(res.Rows)
		if len(got) != len(want) {
			t.Errorf("%q returned %v, want %v", block, got, want)
			continue
		}
		for _, w := range want {
			if !has(got, w) {
				t.Errorf("%q returned %v, missing %q", block, got, w)
			}
		}
	}
}

func TestNumericComparisonIsNumeric(t *testing.T) {
	// Frontmatter is text, and "10" < "9" as a string — which is the kind of
	// wrong that looks like the query working.
	database := projectDB(t)
	got := titles(queries.Run(database, "where: priority > 5", false).Rows)
	if !has(got, "Beta") {
		t.Errorf("priority 10 was compared as a string: %v", got)
	}
}

func TestMissingPropertyDoesNotMatchAComparison(t *testing.T) {
	database := projectDB(t)
	for _, block := range []string{"where: priority > 0", "where: status = active"} {
		if has(titles(queries.Run(database, block, false).Rows), "Delta") {
			t.Errorf("%q matched a note with no such property", block)
		}
	}
	// …but "not equal" does, because a note without the field is not that value.
	if !has(titles(queries.Run(database, "where: status != active", false).Rows), "Delta") {
		t.Error("'!=' excluded a note that has no such property")
	}
}

func TestPropertiesBecomeColumns(t *testing.T) {
	database := projectDB(t)
	res := queries.Run(database, "where: status = active\ncolumns: title, status, priority", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	for _, row := range res.Rows {
		if row["status"] != "active" || row["priority"] == nil {
			t.Errorf("row is missing its properties: %v", row)
		}
	}
}

func TestBuiltInColumnsWinACollision(t *testing.T) {
	// A note with a "path" property must not be able to move itself in a
	// listing — the access filter reads that field.
	root := t.TempDir()
	v, _ := vault.New(root)
	database, err := db.Open(filepath.Join(root, ".grimoire", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ix := index.New(database, v, embed.Hash{})
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Sneaky")
	fm.Set("path", "somewhere/else.md")
	if _, err := v.Write("real.md", "# Sneaky\n", fm); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Reindex(); err != nil {
		t.Fatal(err)
	}
	res := queries.Run(database, "columns: title, path", false)
	if len(res.Rows) != 1 || res.Rows[0]["path"] != "real.md" {
		t.Fatalf("a frontmatter property overrode the real path: %v", res.Rows)
	}
}

func TestFormulaColumns(t *testing.T) {
	database := projectDB(t)
	old := queries.Now
	queries.Now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { queries.Now = old })

	res := queries.Run(database, "where: due exists\n"+
		"formula: overdue_by = days_since(due)\n"+
		"formula: shouting = upper(status)\n"+
		"columns: title, overdue_by, shouting", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	byTitle := map[string]map[string]any{}
	for _, row := range res.Rows {
		byTitle[row["title"].(string)] = row
	}
	if got := byTitle["Beta"]["overdue_by"]; got != 19 {
		t.Errorf("days_since(2026-08-01) = %v, want 19", got)
	}
	// Truncated toward zero rather than floored, so "11 days ago" and
	// "11 days until" are the same number with opposite signs.
	if got := byTitle["Alpha"]["overdue_by"]; got != -11 {
		t.Errorf("days_since(2026-09-01) = %v, want -11", got)
	}
	if byTitle["Beta"]["shouting"] != "DONE" {
		t.Errorf("upper(status) = %v", byTitle["Beta"]["shouting"])
	}
}

func TestDaysSinceAndDaysUntilAreMirrors(t *testing.T) {
	database := projectDB(t)
	old := queries.Now
	queries.Now = func() time.Time { return time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { queries.Now = old })

	res := queries.Run(database, "where: due exists\n"+
		"formula: since = days_since(due)\n"+
		"formula: until = days_until(due)", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	for _, row := range res.Rows {
		since, until := row["since"].(int), row["until"].(int)
		if since != -until {
			t.Errorf("%v: since=%d until=%d are not mirrors", row["title"], since, until)
		}
	}
}

func TestFormulaOnAMissingFieldIsBlankNotAnError(t *testing.T) {
	// One unparseable note should not blank a whole table.
	database := projectDB(t)
	res := queries.Run(database, "formula: age = days_since(due)\ncolumns: title, age", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	for _, row := range res.Rows {
		if row["title"] == "Delta" && row["age"] != "" {
			t.Errorf("a note with no due date computed %v", row["age"])
		}
	}
}

func TestFormulaValidation(t *testing.T) {
	database := projectDB(t)
	for _, block := range []string{
		"formula: age",
		"formula: age = updated",
		"formula: age = rm_rf(updated)",
		"formula: age = days_since()",
		"formula: age = days_since(a, b)",
		"formula: = days_since(updated)",
	} {
		if res := queries.Run(database, block, false); len(res.Errors) == 0 {
			t.Errorf("%q was accepted", block)
		}
	}
}

func TestDefaultAndConcatFormulas(t *testing.T) {
	database := projectDB(t)
	res := queries.Run(database, `formula: who = default(owner, "unassigned")`+"\n"+
		`formula: label = concat(title, " ")`+"\ncolumns: title, who, label", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	for _, row := range res.Rows {
		if row["title"] == "Delta" && row["who"] != "unassigned" {
			t.Errorf("default() did not fill in: %v", row)
		}
		if row["title"] == "Alpha" && row["who"] != "Ana Diaz" {
			t.Errorf("default() overrode a real value: %v", row)
		}
	}
}

func TestGroupBy(t *testing.T) {
	database := projectDB(t)
	res := queries.Run(database, "group_by: status\ncolumns: title, status", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	if res.GroupBy != "status" {
		t.Fatalf("group_by = %q", res.GroupBy)
	}
	counts := map[string]int{}
	for _, g := range res.Groups {
		counts[g.Value] = g.Count
	}
	if counts["active"] != 2 || counts["done"] != 1 {
		t.Errorf("groups = %v", counts)
	}
	// A note missing the field lands in one bucket rather than being dropped —
	// that is usually exactly what someone grouping by it is looking for.
	if counts[""] != 1 {
		t.Errorf("the note with no status was dropped: %v", counts)
	}
	if len(res.Rows) != 4 {
		t.Errorf("grouping changed the row set: %d", len(res.Rows))
	}
}

func TestGroupByAFormula(t *testing.T) {
	database := projectDB(t)
	res := queries.Run(database, "formula: shouting = upper(status)\ngroup_by: shouting", false)
	if len(res.Errors) > 0 {
		t.Fatalf("errored: %v", res.Errors)
	}
	var found bool
	for _, g := range res.Groups {
		if g.Value == "ACTIVE" && g.Count == 2 {
			found = true
		}
	}
	if !found {
		t.Errorf("groups = %+v", res.Groups)
	}
}

func TestPropertyNamesCannotInject(t *testing.T) {
	// The design rule of this package: nothing user-authored becomes SQL. A
	// property name travels into a json_extract path, so it is pattern-checked
	// before it goes anywhere.
	database := projectDB(t)
	for _, block := range []string{
		`where: $.status') OR 1=1 -- = x`,
		`where: "a.b" = x`,
		"where: status.sub = x",
		"group_by: $.status",
		"columns: title, $.status",
	} {
		res := queries.Run(database, block, false)
		if len(res.Errors) == 0 {
			t.Errorf("%q was accepted", block)
		}
	}
}

func TestPropertyValuesCannotInject(t *testing.T) {
	database := projectDB(t)
	for _, value := range []string{"' OR 1=1 --", "%", "_", `\`} {
		res := queries.Run(database, "where: status = "+value, false)
		if len(res.Errors) > 0 {
			t.Fatalf("value %q errored: %v", value, res.Errors)
		}
		if len(res.Rows) != 0 {
			t.Errorf("value %q matched %d rows, want none", value, len(res.Rows))
		}
	}
}

func TestWhereIsRejectedAgainstLines(t *testing.T) {
	// A line has no frontmatter, so this would match nothing forever and look
	// like a query with no results.
	database := projectDB(t)
	if res := queries.Run(database, "from: tasks\nwhere: status = active", false); len(res.Errors) == 0 {
		t.Error("a property filter was accepted against lines")
	}
}

func TestMalformedWhereIsAnError(t *testing.T) {
	database := projectDB(t)
	for _, block := range []string{
		"where: status", "where: = active", "where: status =", "where: owner contains",
	} {
		if res := queries.Run(database, block, false); len(res.Errors) == 0 {
			t.Errorf("%q was accepted", block)
		}
	}
}
