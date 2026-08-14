// Package markdown parses YAML-ish frontmatter, [[wiki-links]], #tags and titles.
//
// Port of server/markdown.py. Intentionally dependency-light on both sides — a
// small forgiving frontmatter parser rather than a real YAML library, so the
// two implementations can be kept identical by inspection.
//
// Two translation hazards worth naming, since both silently corrupt data rather
// than failing loudly:
//
//   - Go's regexp is RE2 and has no lookbehind, so Python's `(?<=\s)#tag` is
//     rewritten as a consuming alternation with the tag in group 2.
//   - Go's \w is ASCII-only while Python's is Unicode-aware. Character classes
//     that can meet non-ASCII text use explicit \p{L}\p{M}\p{N} instead, or a
//     tag like #café would truncate at the accent.
package markdown

import (
	"regexp"
	"strings"
)

var (
	frontmatterRE = regexp.MustCompile(`(?s)^---\s*\n(.*?)\n---\s*\n?`)
	wikilinkRE    = regexp.MustCompile(`\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]`)
	// a #tag: not inside a word, not a markdown heading. Group 2 is the tag —
	// group 1 absorbs the required start-or-space that Python matched by lookbehind.
	tagRE = regexp.MustCompile(`(^|\s)#(\p{L}[\p{L}\p{M}\p{N}_/-]*)`)
	h1RE  = regexp.MustCompile(`(?m)^#[ \t]+(.+?)[ \t]*$`)
	// a fenced code block or inline code — tags/links inside are ignored
	codeFenceRE = regexp.MustCompile("(?s)```.*?```|`[^`]*`")
	headingRE   = regexp.MustCompile(`(?m)^#{1,6}\s.*$`)
	listItemRE  = regexp.MustCompile(`^\s*-\s+`)
	keyValueRE  = regexp.MustCompile(`^([A-Za-z0-9_\-]+):\s*(.*)$`)
)

// Value is a frontmatter value: string, bool, or []Value for lists.
type Value any

// Frontmatter preserves key order, which Python dicts do and Go maps do not.
// Order is observable: it drives serialization back to disk.
type Frontmatter struct {
	keys []string
	vals map[string]Value
}

func NewFrontmatter() *Frontmatter {
	return &Frontmatter{vals: map[string]Value{}}
}

func (f *Frontmatter) Keys() []string { return append([]string(nil), f.keys...) }
func (f *Frontmatter) Len() int       { return len(f.keys) }

func (f *Frontmatter) Get(k string) (Value, bool) {
	v, ok := f.vals[k]
	return v, ok
}

func (f *Frontmatter) Set(k string, v Value) {
	if _, exists := f.vals[k]; !exists {
		f.keys = append(f.keys, k)
	}
	f.vals[k] = v
}

func (f *Frontmatter) Delete(k string) {
	if _, exists := f.vals[k]; !exists {
		return
	}
	delete(f.vals, k)
	for i, key := range f.keys {
		if key == k {
			f.keys = append(f.keys[:i], f.keys[i+1:]...)
			break
		}
	}
}

// Clone returns an independent copy, preserving order.
func (f *Frontmatter) Clone() *Frontmatter {
	c := NewFrontmatter()
	for _, k := range f.keys {
		c.Set(k, f.vals[k])
	}
	return c
}

// StringVal returns the value as a string, or "" when absent or not a scalar.
func (f *Frontmatter) StringVal(k string) string {
	v, ok := f.vals[k]
	if !ok {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	}
	return ""
}

// BoolVal reports whether the key holds a truthy value, matching Python's
// bool() on the parsed scalar: true for `true`, and for any non-empty string.
func (f *Frontmatter) BoolVal(k string) bool {
	v, ok := f.vals[k]
	if !ok {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return t != ""
	case []Value:
		return len(t) > 0
	}
	return false
}

// ParseFrontmatter returns the frontmatter and the remaining body. Missing or
// blank frontmatter yields an empty block and the original text.
func ParseFrontmatter(text string) (*Frontmatter, string) {
	loc := frontmatterRE.FindStringSubmatchIndex(text)
	if loc == nil {
		return NewFrontmatter(), text
	}
	block := text[loc[2]:loc[3]]
	return parseYAMLish(block), text[loc[1]:]
}

func parseYAMLish(block string) *Frontmatter {
	out := NewFrontmatter()
	key := ""
	hasKey := false
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimRight(raw, " \t\r\n\v\f")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if listItemRE.MatchString(line) && hasKey {
			item := scalar(listItemRE.ReplaceAllString(line, ""))
			cur, _ := out.Get(key)
			lst, isList := cur.([]Value)
			if !isList {
				lst = []Value{}
			}
			out.Set(key, append(lst, item))
			continue
		}
		m := keyValueRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		key, hasKey = m[1], true
		val := strings.TrimSpace(m[2])
		switch {
		case val == "":
			out.Set(key, []Value{}) // a list follows, or an empty value
		case strings.HasPrefix(val, "[") && strings.HasSuffix(val, "]"):
			inner := strings.TrimSpace(val[1 : len(val)-1])
			items := []Value{}
			if inner != "" {
				for _, part := range strings.Split(inner, ",") {
					items = append(items, scalar(strings.TrimSpace(part)))
				}
			}
			out.Set(key, items)
		default:
			out.Set(key, scalar(val))
		}
	}
	// collapse empty-list placeholders that never got items into ""
	for _, k := range out.Keys() {
		if lst, ok := out.vals[k].([]Value); ok && len(lst) == 0 && !wasList(block, k) {
			out.vals[k] = ""
		}
	}
	return out
}

func wasList(block, key string) bool {
	q := regexp.QuoteMeta(key)
	if regexp.MustCompile(`(?m)^` + q + `:\s*\n\s*-\s`).MatchString(block) {
		return true
	}
	return regexp.MustCompile(`(?m)^` + q + `:\s*\[`).MatchString(block)
}

func scalar(v string) Value {
	v = strings.Trim(strings.TrimSpace(v), `"`)
	v = strings.Trim(v, "'")
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	}
	return v
}

func stripCode(body string) string {
	return codeFenceRE.ReplaceAllString(body, " ")
}

// Link is one wiki-link occurrence.
type Link struct {
	Target string `json:"target"`
	Alias  string `json:"alias"`
}

// ExtractLinks returns wiki-links in document order, deduplicated by target,
// ignoring anything inside code spans.
func ExtractLinks(body string) []Link {
	seen := map[string]bool{}
	out := []Link{}
	for _, m := range wikilinkRE.FindAllStringSubmatch(stripCode(body), -1) {
		target := strings.TrimSpace(m[1])
		// drop a #heading or ^block anchor for link *resolution*
		base := target
		if i := strings.Index(base, "#"); i >= 0 {
			base = base[:i]
		}
		if i := strings.Index(base, "^"); i >= 0 {
			base = base[:i]
		}
		base = strings.TrimSpace(base)
		if base == "" {
			base = target
		}
		if seen[strings.ToLower(base)] {
			continue
		}
		seen[strings.ToLower(base)] = true
		out = append(out, Link{Target: base, Alias: strings.TrimSpace(m[2])})
	}
	return out
}

// ExtractTags returns #tags in document order, deduplicated case-insensitively,
// ignoring code spans and markdown headings.
func ExtractTags(body string) []string {
	stripped := headingRE.ReplaceAllString(stripCode(body), "")
	seen := map[string]bool{}
	out := []string{}
	for _, m := range tagRE.FindAllStringSubmatch(stripped, -1) {
		t := m[2]
		if seen[strings.ToLower(t)] {
			continue
		}
		seen[strings.ToLower(t)] = true
		out = append(out, t)
	}
	return out
}

// DeriveTitle prefers frontmatter title, then the first H1, then the filename.
func DeriveTitle(fm *Frontmatter, body, filenameStem string) string {
	if fm != nil {
		if s := fm.StringVal("title"); s != "" {
			return s
		}
	}
	if m := h1RE.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(m[1])
	}
	return filenameStem
}
