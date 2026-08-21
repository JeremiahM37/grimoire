package index

import (
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Headings, list items and tasks as rows.
//
// The task list used to be built by reading every note body on every request
// and scanning it for checkboxes, which is linear in the whole vault per call
// and cannot be filtered in SQL. Headings were not addressable at all. Both
// are rows now, derived from the markdown and rebuilt by a reindex like
// everything else in this package.

// Block is one indexed line, with the note it came from.
type Block struct {
	markdown.Block
	Note  string `json:"note"`
	Title string `json:"title"`
	// Provenance from the block's note. A task list is note content: an issue
	// in a connected tracker can contain "- [ ] drop the prod database", and a
	// person scanning their own task view should be able to see that it is not
	// theirs.
	Origin string `json:"origin,omitempty"`
	Trust  string `json:"trust"`
}

// BlockQuery selects blocks.
type BlockQuery struct {
	Filter Filter

	// Kind is heading, item or task. Empty means any.
	Kind string
	// Note restricts to one note; Path restricts to a prefix.
	Note string
	Path string
	// Text matches case-insensitively as a substring — blocks are single
	// lines, so a phrase search over them is a substring search.
	Text string
	// Section restricts to blocks under a heading of this name.
	Section string
	// Level, when non-zero, restricts to that heading depth or list depth.
	Level int
	// Checked filters tasks; nil means both.
	Checked *bool

	Limit int
}

func (ix *Index) writeBlockRows(note *vault.Note) error {
	if err := ix.DB.Exec("DELETE FROM blocks WHERE note=?", note.Path); err != nil {
		return err
	}
	if note.Encrypted {
		// Never mine structure out of ciphertext, for the same reason the
		// body is not indexed: the point of an encrypted note is that the
		// index cannot read it.
		return nil
	}
	private := 0
	if note.Private {
		private = 1
	}
	space := ix.spaceOf(note.Path)
	acl := EncodeACL(splitList(note.Frontmatter.StringVal("readers")))
	for _, b := range markdown.ParseBlocks(note.Body) {
		checked := 0
		if b.Checked {
			checked = 1
		}
		if err := ix.DB.Exec(
			"INSERT INTO blocks(note,kind,text,level,line,checked,parent,private,space,acl)"+
				" VALUES(?,?,?,?,?,?,?,?,?,?)",
			note.Path, b.Kind, b.Text, b.Level, b.Line, checked, b.Parent,
			private, space, acl); err != nil {
			return err
		}
	}
	return nil
}

// Blocks returns the blocks a principal may read.
//
// Access is applied in SQL rather than to the output, the same way ranking
// applies it: a block carries its note's space and reader list, denormalized
// onto the row, so a listing cannot hand back a line from a note the caller
// cannot open.
func (ix *Index) Blocks(q BlockQuery) ([]Block, error) {
	if q.Limit <= 0 {
		q.Limit = 200
	}
	where := []string{"1=1"}
	var args []any
	if q.Kind != "" {
		where = append(where, "b.kind=?")
		args = append(args, q.Kind)
	}
	if q.Note != "" {
		where = append(where, "b.note=?")
		args = append(args, q.Note)
	}
	if q.Path != "" {
		where = append(where, `b.note LIKE ? ESCAPE '\'`)
		args = append(args, likePrefix(q.Path))
	}
	if q.Text != "" {
		where = append(where, "b.text LIKE ? ESCAPE '\\' COLLATE NOCASE")
		args = append(args, "%"+likeEscape(q.Text)+"%")
	}
	if q.Section != "" {
		where = append(where, "b.parent=? COLLATE NOCASE")
		args = append(args, q.Section)
	}
	if q.Level > 0 {
		where = append(where, "b.level=?")
		args = append(args, q.Level)
	}
	if q.Checked != nil {
		want := 0
		if *q.Checked {
			want = 1
		}
		where = append(where, "b.checked=?")
		args = append(args, want)
	}
	if !q.Filter.IncludePrivate {
		where = append(where, "b.private=0")
	}
	// The blocks table carries no trust column of its own — a block belongs to
	// a note, and the note owns the provenance — so this joins. It is here at
	// all because Filter is built in ONE place for every content route: a
	// caller that passes trusted=1 to /api/tasks and silently gets a GitHub
	// issue's checklist back would be worse off than if the parameter did not
	// exist, since it would believe it had asked.
	if q.Filter.TrustedOnly {
		where = append(where, "COALESCE(n.untrusted,0)=0")
	}
	if q.Filter.Spaces != nil {
		names := make([]string, 0, len(q.Filter.Spaces))
		for name, ok := range q.Filter.Spaces {
			if ok {
				names = append(names, name)
			}
		}
		if len(names) == 0 {
			return []Block{}, nil
		}
		where = append(where, "b.space IN ("+placeholders(len(names))+")")
		for _, n := range names {
			args = append(args, n)
		}
	}

	rows, err := ix.DB.Query(
		"SELECT b.note, b.kind, b.text, b.level, b.line, b.checked, b.parent, b.acl, "+
			"COALESCE(n.title,''), COALESCE(n.origin,''), COALESCE(n.untrusted,0) "+
			"FROM blocks b LEFT JOIN notes n ON n.path=b.note WHERE "+
			strings.Join(where, " AND ")+
			" ORDER BY b.note, b.line LIMIT ?", append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Block{}
	for rows.Next() {
		var b Block
		var checked, untrusted int
		var acl string
		if err := rows.Scan(&b.Note, &b.Kind, &b.Text, &b.Level, &b.Line,
			&checked, &b.Parent, &acl, &b.Title, &b.Origin, &untrusted); err != nil {
			return nil, err
		}
		b.Trust = levelName(untrusted != 0)
		b.Checked = checked == 1
		if !q.Filter.IgnoreACLs && !aclAllows(acl, q.Filter.User) {
			continue
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// likeEscape neutralizes LIKE metacharacters so a search for "50%" matches
// literally rather than matching everything.
func likeEscape(s string) string {
	e := strings.ReplaceAll(s, `\`, `\\`)
	e = strings.ReplaceAll(e, "%", `\%`)
	return strings.ReplaceAll(e, "_", `\_`)
}

func likePrefix(prefix string) string { return likeEscape(prefix) + "%" }
