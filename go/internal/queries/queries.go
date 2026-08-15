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
)

// Spec is a parsed, validated query block. All filters are optional.
type Spec struct {
	Tag      *string
	Path     *string
	Text     *string
	LinkedTo *string
	Pinned   *bool
	Sort     string
	SortDesc bool
	Limit    int
	Render   string
	Columns  []string
	Errors   []string
}

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
		Sort: "updated", SortDesc: true, Limit: DefaultLimit,
		Render: "list", Columns: []string{"title", "updated"}, Errors: []string{},
	}
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
			parts := strings.Fields(strings.ToLower(val))
			if len(parts) == 0 || !sortFields[parts[0]] {
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
			var cols, bad []string
			for _, c := range strings.Split(val, ",") {
				c = strings.ToLower(strings.TrimSpace(c))
				if c == "" {
					continue
				}
				if !columnsAllowed[c] {
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
	return spec
}

// Execute runs a validated spec. It returns display fields only — never bodies,
// so a query block cannot become a read-everything gadget on the
// unauthenticated surfaces.
func Execute(database *db.DB, spec *Spec, includePrivate bool) ([]map[string]any, error) {
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

// likePrefix escapes LIKE metacharacters so a path prefix matches literally.
func likePrefix(prefix string) string {
	e := strings.ReplaceAll(prefix, `\`, `\\`)
	e = strings.ReplaceAll(e, "%", `\%`)
	e = strings.ReplaceAll(e, "_", `\_`)
	return e + "%"
}

func asMDPath(s string) string {
	if strings.HasSuffix(s, ".md") {
		return s
	}
	return s + ".md"
}
