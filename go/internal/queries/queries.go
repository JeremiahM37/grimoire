// Package queries runs ```query blocks against the index.
//
// Port of server/queries.py. A query block is user-authored text that becomes
// SQL, so the whole design is about never letting it become injection:
//
//   - every identifier that can reach generated SQL comes from a whitelist
//     (sort fields, columns, render modes); values are always bound parameters;
//   - free text goes to FTS5 as a single QUOTED phrase, so MATCH syntax
//     (NEAR/OR/column filters) cannot be smuggled through it;
//   - results are capped, so a hostile block cannot dump the vault;
//   - parsing is forgiving: unknown keys become errors in the output rather
//     than exceptions, because a broken block should explain itself, not take
//     the page down.
package queries

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/fts"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

const (
	DefaultLimit = 50
	MaxLimit     = 200
)

var (
	sortFields = map[string]bool{
		"title": true, "updated": true, "created": true, "path": true, "mtime": true}
	columnsAllowed = map[string]bool{
		"title": true, "path": true, "updated": true, "created": true, "tags": true}
	renders = map[string]bool{"list": true, "table": true, "count": true}

	// Sources a block can be drawn from. "notes" is the default and the
	// original behaviour; the rest query the blocks table — the lines inside
	// notes — which is what makes "every open task" and "every heading called
	// Decisions" expressible rather than requiring someone to scroll.
	sources = map[string]string{
		"notes": "", "headings": "heading", "items": "item", "tasks": "task",
	}
	blockSortFields = map[string]bool{
		"note": true, "path": true, "line": true, "text": true, "level": true}
	blockColumns = map[string]bool{
		"text": true, "note": true, "path": true, "title": true, "line": true,
		"level": true, "section": true, "checked": true, "kind": true}
)

// Spec is a parsed, validated query block. All filters are optional.
type Spec struct {
	// From selects what a row IS: a note, or a line inside one.
	From     string
	Tag      *string
	Path     *string
	Text     *string
	LinkedTo *string
	Pinned   *bool

	// Block filters. Meaningful only when From is not "notes"; using one
	// against notes is an error rather than silence, since silently ignoring
	// a filter shows the wrong rows and looks like the query working.
	Checked *bool
	Section *string
	Level   int

	Sort     string
	SortDesc bool
	Limit    int
	Render   string
	Columns  []string
	Errors   []string
}

// Blocks reports whether this query is over lines rather than notes.
func (s *Spec) Blocks() bool { return s.From != "" && s.From != "notes" }

// Result is the shape renderers and /api/query consume.
type Result struct {
	Render  string           `json:"render"`
	Columns []string         `json:"columns"`
	Rows    []map[string]any `json:"rows"`
	Count   int              `json:"count"`
	Errors  []string         `json:"errors"`
}

// Parse reads the text inside a ```query fence.
func Parse(block string) *Spec {
	spec := &Spec{
		From: "notes", Sort: "updated", SortDesc: true, Limit: DefaultLimit,
		Render: "list", Columns: []string{"title", "updated"}, Errors: []string{},
	}
	// Whether the caller chose columns and a sort, so the defaults for a block
	// query can differ without overriding an explicit choice.
	var setColumns, setSort bool
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, _ := strings.Cut(line, ":")
		key = strings.ReplaceAll(strings.ToLower(strings.TrimSpace(key)), "-", "_")
		val = strings.TrimSpace(val)
		if val == "" && key != "" {
			spec.Errors = append(spec.Errors, fmt.Sprintf("missing value for '%s'", key))
			continue
		}
		switch key {
		case "from":
			if _, ok := sources[strings.ToLower(val)]; !ok {
				spec.Errors = append(spec.Errors, fmt.Sprintf(
					"unknown source '%s' (use notes|headings|items|tasks)", val))
				break
			}
			spec.From = strings.ToLower(val)
		case "checked", "done":
			v := truthy(val)
			spec.Checked = &v
		case "section":
			v := val
			spec.Section = &v
		case "level":
			n, err := strconv.Atoi(val)
			if err != nil || n < 1 {
				spec.Errors = append(spec.Errors,
					fmt.Sprintf("level must be a positive number, got '%s'", val))
				break
			}
			spec.Level = n
		case "tag":
			v := strings.TrimPrefix(val, "#")
			spec.Tag = &v
		case "path":
			v := val
			spec.Path = &v
		case "text":
			v := val
			spec.Text = &v
		case "linked_to":
			// accept "[[Title]]", "Title" or a path
			v := strings.TrimSpace(strings.Trim(val, "[]"))
			spec.LinkedTo = &v
		case "pinned":
			v := val == "true" || val == "yes" || val == "1" ||
				strings.EqualFold(val, "true") || strings.EqualFold(val, "yes")
			spec.Pinned = &v
		case "sort":
			setSort = true
			parts := strings.Fields(strings.ToLower(val))
			if len(parts) == 0 || !(sortFields[parts[0]] || blockSortFields[parts[0]]) {
				field := val
				if len(parts) > 0 {
					field = parts[0]
				}
				spec.Errors = append(spec.Errors, fmt.Sprintf("unknown sort field '%s'", field))
				break
			}
			spec.Sort = parts[0]
			spec.SortDesc = len(parts) < 2 || parts[1] != "asc"
		case "limit":
			n, err := strconv.Atoi(val)
			if err != nil {
				spec.Errors = append(spec.Errors,
					fmt.Sprintf("limit must be a number, got '%s'", val))
				break
			}
			if n < 1 {
				n = 1
			}
			if n > MaxLimit {
				n = MaxLimit
			}
			spec.Limit = n
		case "render":
			if !renders[strings.ToLower(val)] {
				spec.Errors = append(spec.Errors,
					fmt.Sprintf("unknown render '%s' (use list|table|count)", val))
				break
			}
			spec.Render = strings.ToLower(val)
		case "columns":
			setColumns = true
			var cols, bad []string
			for _, c := range strings.Split(val, ",") {
				c = strings.ToLower(strings.TrimSpace(c))
				if c == "" {
					continue
				}
				if !columnsAllowed[c] && !blockColumns[c] {
					bad = append(bad, c)
					continue
				}
				cols = append(cols, c)
			}
			if len(bad) > 0 {
				spec.Errors = append(spec.Errors,
					"unknown column(s): "+strings.Join(bad, ", "))
			}
			if len(cols) > 0 {
				spec.Columns = cols
			}
		default:
			spec.Errors = append(spec.Errors, fmt.Sprintf("unknown key '%s'", key))
		}
	}
	spec.finish(setColumns, setSort)
	return spec
}

// finish applies the defaults a source implies and rejects filters that mean
// nothing for it.
func (s *Spec) finish(setColumns, setSort bool) {
	if !s.Blocks() {
		for name, used := range map[string]bool{
			"checked": s.Checked != nil, "section": s.Section != nil,
			"level": s.Level > 0,
		} {
			if used {
				s.Errors = append(s.Errors, fmt.Sprintf(
					"'%s' needs 'from: tasks', 'items' or 'headings'", name))
			}
		}
		return
	}
	if s.Checked != nil && s.From != "tasks" {
		s.Errors = append(s.Errors, "'checked' only applies to 'from: tasks'")
	}
	// A line has no title and no updated date of its own, so the note-shaped
	// defaults would render a table of blanks.
	if !setColumns {
		s.Columns = []string{"text", "title"}
	}
	if !setSort {
		s.Sort, s.SortDesc = "note", false
	}
	if !blockSortFields[s.Sort] {
		s.Errors = append(s.Errors, fmt.Sprintf(
			"cannot sort lines by '%s' (use note|line|text|level)", s.Sort))
	}
	for _, c := range s.Columns {
		if !blockColumns[c] {
			s.Errors = append(s.Errors, fmt.Sprintf("lines have no column '%s'", c))
		}
	}
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "1", "done", "checked":
		return true
	}
	return false
}

// Execute runs a validated spec. It returns display fields only — never bodies,
// so a query block cannot become a read-everything gadget on the
// unauthenticated surfaces.
func Execute(database *db.DB, spec *Spec, includePrivate bool) ([]map[string]any, error) {
	if spec.Blocks() {
		return executeBlocks(database, spec, includePrivate)
	}
	return executeNotes(database, spec, includePrivate)
}

// executeBlocks queries the lines inside notes.
//
// Every row carries the note's `path`, which is not decoration: the HTTP layer
// filters query results by that field against what the caller may read, so a
// block row goes through exactly the same check a note row does rather than
// through a second copy of the rule.
func executeBlocks(database *db.DB, spec *Spec, includePrivate bool) ([]map[string]any, error) {
	where := []string{"1=1"}
	var params []any
	if kind := sources[spec.From]; kind != "" {
		where = append(where, "b.kind = ?")
		params = append(params, kind)
	}
	if !includePrivate {
		where = append(where, "b.private = 0")
	}
	if spec.Path != nil {
		where = append(where, `b.note LIKE ? ESCAPE '\'`)
		params = append(params, likePrefix(*spec.Path))
	}
	if spec.Text != nil {
		// A block is one line, so a phrase search over it is a substring
		// search — and the metacharacters are escaped so "50%" means "50%".
		where = append(where, `b.text LIKE ? ESCAPE '\' COLLATE NOCASE`)
		params = append(params, "%"+likeEscape(*spec.Text)+"%")
	}
	if spec.Section != nil {
		where = append(where, "b.parent = ? COLLATE NOCASE")
		params = append(params, *spec.Section)
	}
	if spec.Level > 0 {
		where = append(where, "b.level = ?")
		params = append(params, spec.Level)
	}
	if spec.Checked != nil {
		want := 0
		if *spec.Checked {
			want = 1
		}
		where = append(where, "b.checked = ?")
		params = append(params, want)
	}
	if spec.Tag != nil {
		where = append(where, "b.note IN (SELECT note FROM tags WHERE tag = ? COLLATE NOCASE)")
		params = append(params, *spec.Tag)
	}
	if spec.LinkedTo != nil {
		where = append(where,
			"b.note IN (SELECT src FROM links WHERE resolved=1 AND "+
				"(dst = ? OR target = ? COLLATE NOCASE))")
		params = append(params, asMDPath(*spec.LinkedTo), *spec.LinkedTo)
	}

	dir := "DESC"
	if !spec.SortDesc {
		dir = "ASC"
	}
	// spec.Sort is whitelisted in finish(), so this interpolation cannot
	// inject. "path" is an alias for the note it lives in, since that is what
	// a person means when they sort a task list by file.
	sortCol := "b." + spec.Sort
	if spec.Sort == "path" {
		sortCol = "b.note"
	}
	sql := "SELECT b.note, b.kind, b.text, b.level, b.line, b.checked, b.parent, " +
		"COALESCE(n.title,'') FROM blocks b LEFT JOIN notes n ON n.path = b.note WHERE " +
		strings.Join(where, " AND ") +
		fmt.Sprintf(" ORDER BY %s %s, b.note, b.line LIMIT ?", sortCol, dir)
	params = append(params, spec.Limit)

	rows, err := database.Query(sql, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var note, kind, text, parent, title string
		var level, line, checked int
		if err := rows.Scan(&note, &kind, &text, &level, &line, &checked,
			&parent, &title); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			// `path` is the note, so the access filter finds it where it
			// looks for it; `note` is the same value under the name a person
			// writing a column list reaches for.
			"path": note, "note": note, "kind": kind, "text": text,
			"level": level, "line": line, "checked": checked == 1,
			"section": parent, "title": title,
		})
	}
	return out, rows.Err()
}

func executeNotes(database *db.DB, spec *Spec, includePrivate bool) ([]map[string]any, error) {
	var where []string
	var params []any

	if !includePrivate {
		where = append(where, "n.private = 0")
	}
	if spec.Tag != nil {
		where = append(where, "n.path IN (SELECT note FROM tags WHERE tag = ? COLLATE NOCASE)")
		params = append(params, *spec.Tag)
	}
	if spec.Path != nil {
		where = append(where, `n.path LIKE ? ESCAPE '\'`)
		params = append(params, likePrefix(*spec.Path))
	}
	if spec.Pinned != nil {
		// pinned lives in frontmatter; the index stores frontmatter_json verbatim
		op := "LIKE"
		if !*spec.Pinned {
			op = "NOT LIKE"
		}
		where = append(where, `n.frontmatter_json `+op+` '%"pinned": true%'`)
	}
	if spec.LinkedTo != nil {
		where = append(where,
			"n.path IN (SELECT src FROM links WHERE resolved=1 AND "+
				"(dst = ? OR target = ? COLLATE NOCASE))")
		params = append(params, asMDPath(*spec.LinkedTo), *spec.LinkedTo)
	}
	if spec.Text != nil {
		where = append(where, "n.path IN (SELECT path FROM fts WHERE fts MATCH ?)")
		params = append(params, fts.Phrase(*spec.Text))
	}

	dir := "DESC"
	if !spec.SortDesc {
		dir = "ASC"
	}
	sql := "SELECT n.path, n.title, n.updated, n.created FROM notes n "
	if len(where) > 0 {
		sql += "WHERE " + strings.Join(where, " AND ")
	}
	// spec.Sort is whitelisted above, so this interpolation cannot inject
	sql += fmt.Sprintf(" ORDER BY n.%s %s, n.path LIMIT ?", spec.Sort, dir)
	params = append(params, spec.Limit)

	rows, err := database.Query(sql, params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []map[string]any{}
	for rows.Next() {
		var path, title, updated, created string
		if err := rows.Scan(&path, &title, &updated, &created); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{
			"path": path, "title": title, "updated": updated, "created": created})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	wantTags := spec.Render == "table"
	for _, c := range spec.Columns {
		if c == "tags" {
			wantTags = true
		}
	}
	if wantTags {
		for _, r := range out {
			tags := []string{}
			trows, err := database.Query("SELECT tag FROM tags WHERE note=? ORDER BY tag",
				r["path"])
			if err != nil {
				continue
			}
			for trows.Next() {
				var t string
				if trows.Scan(&t) == nil {
					tags = append(tags, t)
				}
			}
			if err := trows.Err(); err != nil {
				continue
			}
			trows.Close()
			r["tags"] = tags
		}
	}
	return out, nil
}

// Run parses and executes in one step.
func Run(database *db.DB, block string, includePrivate bool) *Result {
	spec := Parse(block)
	rows := []map[string]any{}
	if len(spec.Errors) == 0 {
		if got, err := Execute(database, spec, includePrivate); err == nil {
			rows = got
		} else {
			spec.Errors = append(spec.Errors, err.Error())
		}
	}
	return &Result{
		Render: spec.Render, Columns: spec.Columns,
		Rows: rows, Count: len(rows), Errors: spec.Errors,
	}
}

// likeEscape neutralizes LIKE metacharacters so a search for "50%" matches
// literally rather than matching everything.
func likeEscape(s string) string {
	e := strings.ReplaceAll(s, `\`, `\\`)
	e = strings.ReplaceAll(e, "%", `\%`)
	return strings.ReplaceAll(e, "_", `\_`)
}

// likePrefix escapes LIKE metacharacters so a path prefix matches literally.
func likePrefix(prefix string) string { return likeEscape(prefix) + "%" }

func asMDPath(s string) string {
	if strings.HasSuffix(s, ".md") {
		return s
	}
	return s + ".md"
}
