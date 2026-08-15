// Package render turns markdown into safe HTML, mirroring the PWA's client
// renderer (web/markdown.js) so the read surface and HTML export look the same.
// The two are kept in lockstep — a rule added here must land there too, and
// vice versa. Neither can be deleted: this renders /read and the export, that
// one renders previews with no network.
//
// Port of the original server/render.py (see ../../README.md). The HTML is a
// byte-level contract: the vision-checked /read pages and the client renderer
// both depend on this exact output, so the
// goal here is fidelity, not improvement. Where Python does something surprising
// (double-escaping a wiki-link label, for instance) this reproduces it rather
// than quietly correcting it — a "fix" here is a behaviour change nobody asked
// for, and the fixtures would catch it anyway.
//
// Three translation hazards, all silent if you get them wrong:
//
//   - Python's html.escape emits &quot; and &#x27;; Go's html.EscapeString emits
//     &#34; and &#39;. Same meaning, different bytes. escapeHTML below matches
//     Python.
//   - Go's regexp is RE2: no lookahead or lookbehind. The emphasis rule
//     (?<!\*)\*([^*]+)\*(?!\*) is hand-scanned to reproduce the engine's
//     left-to-right retry semantics exactly.
//   - Go's \w is ASCII-only; Python's is Unicode-aware. Anything that can meet
//     a heading or tag in another language uses \p{L}\p{M}\p{N} instead.
package render

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// MaxEmbedDepth limits transclusion: an embedded note may embed another, once.
// Prevents runaway nesting and infinite cycles (a cycle also trips the seen set).
const MaxEmbedDepth = 2

var (
	wikilinkRE       = regexp.MustCompile(`\[\[([^\[\]|]+?)(?:\|([^\[\]]+))?\]\]`)
	embedRE          = regexp.MustCompile(`!\[\[([^\[\]|]+?)\]\]`)
	embedFullRE      = regexp.MustCompile(`^!\[\[([^\[\]|]+?)\]\]$`)
	headingRE        = regexp.MustCompile(`^(#{1,6})\s+(.+)$`)
	taskRE           = regexp.MustCompile(`^\s*[-*]\s+\[([ xX])\]\s+(.*)$`)
	uliRE            = regexp.MustCompile(`^\s*[-*]\s+(.*)$`)
	oliRE            = regexp.MustCompile(`^\s*\d+\.\s+(.*)$`)
	footnoteDefRE    = regexp.MustCompile(`^\[\^([\p{L}\p{M}\p{N}_-]+)\]:\s+(.*)$`)
	footnoteRefRE    = regexp.MustCompile(`\[\^([\p{L}\p{M}\p{N}_-]+)\]`)
	calloutRE        = regexp.MustCompile(`^\s*>\s*\[!(\w+)\]\s*(.*)$`)
	quoteLineRE      = regexp.MustCompile(`^\s*>`)
	quoteStripRE     = regexp.MustCompile(`^\s*>\s?`)
	hrRE             = regexp.MustCompile(`^\s*(---|\*\*\*)\s*$`)
	tableSepRE       = regexp.MustCompile(`^\|?[\s:|-]*-[\s:|-]*\|?$`)
	codeSpanRE       = regexp.MustCompile("`([^`]+)`")
	markRE           = regexp.MustCompile(`==([^=]+)==`)
	strikeRE         = regexp.MustCompile(`~~([^~]+)~~`)
	strongRE         = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	linkRE           = regexp.MustCompile(`\[([^\]]+)\]\((https?:[^)]+)\)`)
	tagRE            = regexp.MustCompile(`(^|\s)#(\p{L}[\p{L}\p{M}\p{N}_/-]*)`)
	sentinelRE       = regexp.MustCompile("\x00c(\\d+)\x00")
	headingIDStripRE = regexp.MustCompile(`[^\p{L}\p{N}_\s-]`)
	headingIDDashRE  = regexp.MustCompile(`[\s_]+`)
	emphasisRE       = regexp.MustCompile(`^\*([^*]+)\*`)
)

var imageExts = []string{".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".avif"}

// Context carries everything a render pass may need. All capabilities are
// optional — a zero Context renders plain markdown with unresolved links.
type Context struct {
	LinkMap  map[string]string        // lower title/stem/path -> rel path
	ImgSrc   func(rel string) string  // attachment URL
	NoteBody func(rel string) *string // transclusion source; nil disables it
	RunQuery func(block string) *QueryResult
	LinkHref func(rel string) string

	depth     int
	embedding map[string]bool // cycle guard (rel paths)
}

// QueryResult is the output of a ```query block.
type QueryResult struct {
	Errors  []string
	Rows    []map[string]any
	Columns []string
	Render  string // "list" | "table" | "count"
}

func (c *Context) imgSrc(rel string) string {
	if c.ImgSrc != nil {
		return c.ImgSrc(rel)
	}
	return "/api/file/" + rel
}

func (c *Context) linkHref(rel string) string {
	if c.LinkHref != nil {
		return c.LinkHref(rel)
	}
	return "/read/" + stripMD(rel)
}

func (c *Context) lookup(target string) (string, bool) {
	if c.LinkMap == nil {
		return "", false
	}
	rel, ok := c.LinkMap[strings.ToLower(target)]
	return rel, ok
}

// Render renders markdown to safe HTML with a bare context.
func Render(body string, linkMap map[string]string) string {
	return RenderWith(body, &Context{LinkMap: linkMap})
}

// RenderWith renders markdown using the supplied context.
func RenderWith(body string, ctx *Context) string {
	if ctx == nil {
		ctx = &Context{}
	}
	footnoteKeys, footnotes := collectFootnotes(body)
	lines := strings.Split(body, "\n")
	var out []string
	var listStack []string // "ul" | "ol"

	closeLists := func() {
		for len(listStack) > 0 {
			out = append(out, "</"+listStack[len(listStack)-1]+">")
			listStack = listStack[:len(listStack)-1]
		}
	}

	for i := 0; i < len(lines); {
		raw := lines[i]

		// footnote definitions are rendered once, at the end
		if footnoteDefRE.MatchString(raw) {
			i++
			continue
		}

		// fenced block — code, or a live ```query block
		if strings.HasPrefix(strings.TrimSpace(raw), "```") {
			closeLists()
			lang := strings.TrimSpace(strings.TrimSpace(raw)[3:])
			j := i + 1
			var buf []string
			for j < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[j]), "```") {
				buf = append(buf, lines[j])
				j++
			}
			if lang == "query" && ctx.RunQuery != nil {
				out = append(out, queryHTML(strings.Join(buf, "\n"), ctx))
			} else {
				cls := ""
				if lang != "" {
					cls = ` class="lang-` + escapeHTML(lang) + `"`
				}
				out = append(out, "<pre><code"+cls+">"+
					HighlightCode(strings.Join(buf, "\n"), lang)+"</code></pre>")
			}
			i = j + 1
			continue
		}

		// a callout: > [!type] title  followed by more > lines
		if cm := calloutRE.FindStringSubmatch(raw); cm != nil {
			closeLists()
			kind := strings.ToLower(cm[1])
			title := strings.TrimSpace(cm[2])
			if title == "" {
				title = capitalize(kind)
			}
			j := i + 1
			var quoted []string
			for j < len(lines) && quoteLineRE.MatchString(lines[j]) {
				quoted = append(quoted, quoteStripRE.ReplaceAllString(lines[j], ""))
				j++
			}
			inner := ""
			if len(quoted) > 0 {
				inner = RenderWith(strings.Join(quoted, "\n"), ctx)
			}
			out = append(out, `<div class="callout callout-`+escapeHTML(kind)+`">`+
				`<div class="callout-title">`+inline(title, ctx)+`</div>`+
				`<div class="callout-body">`+inner+`</div></div>`)
			i = j
			continue
		}

		// a table: a header row followed by a |---|---| separator
		if isTableRow(raw) && i+1 < len(lines) && isTableSep(lines[i+1]) {
			closeLists()
			j := i + 2
			var rows []string
			for j < len(lines) && isTableRow(lines[j]) {
				rows = append(rows, lines[j])
				j++
			}
			out = append(out, tableHTML(raw, rows, ctx))
			i = j
			continue
		}

		// a whole-line ![[Note]] → block-level transclusion (images stay inline)
		if em := embedFullRE.FindStringSubmatch(strings.TrimSpace(raw)); em != nil &&
			!isImage(em[1]) && ctx.NoteBody != nil {
			closeLists()
			out = append(out, transclude(strings.TrimSpace(em[1]), ctx))
			i++
			continue
		}

		switch {
		case headingRE.MatchString(raw):
			closeLists()
			h := headingRE.FindStringSubmatch(raw)
			lvl := len(h[1])
			out = append(out, fmt.Sprintf(`<h%d id="%s">%s</h%d>`,
				lvl, HeadingID(h[2]), inline(h[2], ctx), lvl))

		case taskRE.MatchString(raw):
			task := taskRE.FindStringSubmatch(raw)
			if len(listStack) == 0 || listStack[len(listStack)-1] != "ul" {
				closeLists()
				out = append(out, "<ul>")
				listStack = append(listStack, "ul")
			}
			done := strings.ToLower(task[1]) == "x"
			box, cls := "disabled", ""
			if done {
				box, cls = "checked disabled", " class='done'"
			}
			out = append(out, "<li"+cls+"><input type='checkbox' "+box+"> "+
				inline(task[2], ctx)+"</li>")

		case oliRE.MatchString(raw):
			if len(listStack) == 0 || listStack[len(listStack)-1] != "ol" {
				closeLists()
				out = append(out, "<ol>")
				listStack = append(listStack, "ol")
			}
			out = append(out, "<li>"+inline(oliRE.FindStringSubmatch(raw)[1], ctx)+"</li>")

		case uliRE.MatchString(raw):
			if len(listStack) == 0 || listStack[len(listStack)-1] != "ul" {
				closeLists()
				out = append(out, "<ul>")
				listStack = append(listStack, "ul")
			}
			out = append(out, "<li>"+inline(uliRE.FindStringSubmatch(raw)[1], ctx)+"</li>")

		case strings.TrimSpace(raw) == "":
			closeLists()

		case quoteStripRE.MatchString(raw):
			closeLists()
			out = append(out, "<blockquote>"+
				inline(quoteStripRE.ReplaceAllString(raw, ""), ctx)+"</blockquote>")

		case hrRE.MatchString(raw):
			closeLists()
			out = append(out, "<hr>")

		default:
			closeLists()
			out = append(out, "<p>"+inline(raw, ctx)+"</p>")
		}
		i++
	}
	closeLists()
	if len(footnoteKeys) > 0 {
		out = append(out, footnotesHTML(footnoteKeys, footnotes, ctx))
	}
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- inline rules

func inline(text string, ctx *Context) string {
	out := escapeHTML(text)

	// Inline code is literal: stash each span behind a sentinel so the rules
	// below can't reach inside it. \x00 can't occur in escaped text, so the
	// sentinel never collides.
	var spans []string
	out = codeSpanRE.ReplaceAllStringFunc(out, func(m string) string {
		spans = append(spans, codeSpanRE.FindStringSubmatch(m)[1])
		return "\x00c" + strconv.Itoa(len(spans)-1) + "\x00"
	})

	out = embedRE.ReplaceAllStringFunc(out, func(m string) string {
		return embedInline(strings.TrimSpace(embedRE.FindStringSubmatch(m)[1]), ctx)
	})
	out = wikilinkRE.ReplaceAllStringFunc(out, func(m string) string {
		return wikilinkHTML(wikilinkRE.FindStringSubmatch(m), ctx)
	})
	out = footnoteRefRE.ReplaceAllString(out,
		`<sup class="fn-ref" id="fnref-$1"><a href="#fn-$1">$1</a></sup>`)
	out = markRE.ReplaceAllString(out, "<mark>$1</mark>")
	out = strikeRE.ReplaceAllString(out, "<del>$1</del>")
	out = strongRE.ReplaceAllString(out, "<strong>$1</strong>")
	out = replaceEmphasis(out)
	out = linkRE.ReplaceAllString(out, `<a href="$2" target="_blank" rel="noopener">$1</a>`)
	out = tagRE.ReplaceAllString(out, `$1<span class="tag">#$2</span>`)

	return sentinelRE.ReplaceAllStringFunc(out, func(m string) string {
		idx, _ := strconv.Atoi(sentinelRE.FindStringSubmatch(m)[1])
		return "<code>" + spans[idx] + "</code>"
	})
}

// replaceEmphasis implements (?<!\*)\*([^*]+)\*(?!\*).
//
// RE2 has no lookaround, and simply filtering matches afterwards is not
// equivalent: when a lookaround fails, Python's engine advances one character
// and retries, which can find a *different* match than skipping the candidate
// wholesale. This scans the same way the engine does.
func replaceEmphasis(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		if s[i] != '*' {
			b.WriteByte(s[i])
			i++
			continue
		}
		// (?<!\*): the character before the opening star must not be a star
		if i > 0 && s[i-1] == '*' {
			b.WriteByte(s[i])
			i++
			continue
		}
		m := emphasisRE.FindStringSubmatchIndex(s[i:])
		if m == nil {
			b.WriteByte(s[i])
			i++
			continue
		}
		end := i + m[1] // index just past the closing star
		// (?!\*): the character after the closing star must not be a star
		if end < len(s) && s[end] == '*' {
			b.WriteByte(s[i])
			i++
			continue
		}
		b.WriteString("<em>" + s[i+m[2]:i+m[3]] + "</em>")
		i = end
	}
	return b.String()
}

func embedInline(target string, ctx *Context) string {
	if isImage(target) {
		return `<img src="` + escapeHTML(ctx.imgSrc(target)) + `" alt="` +
			escapeHTML(target) + `">`
	}
	base := strings.TrimSpace(strings.SplitN(target, "#", 2)[0])
	if rel, ok := ctx.lookup(base); ok {
		return `<a class="wikilink" href="` + escapeHTML(ctx.linkHref(rel)) + `">` +
			escapeHTML(target) + `</a>`
	}
	return `<span class="unresolved">` + escapeHTML(target) + `</span>`
}

func wikilinkHTML(m []string, ctx *Context) string {
	rawTarget := strings.TrimSpace(m[1])
	base, anchor, _ := strings.Cut(rawTarget, "#")
	label := m[2]
	if label == "" {
		label = rawTarget
	}
	label = escapeHTML(label)

	if dst, ok := ctx.lookup(strings.TrimSpace(base)); ok {
		href := ctx.linkHref(dst)
		if anchor != "" {
			href += "#" + HeadingID(anchor)
		}
		return `<a class="wikilink" href="` + escapeHTML(href) + `">` + label + `</a>`
	}
	return `<span class="unresolved">` + label + `</span>`
}

// ------------------------------------------------------- footnotes & headings

// HeadingID builds a stable, url-safe anchor. Shared by links and headings so
// [[note#My Heading]] scrolls to <h2 id="h-my-heading">.
func HeadingID(text string) string {
	slug := headingIDStripRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(text)), "")
	slug = strings.Trim(headingIDDashRE.ReplaceAllString(slug, "-"), "-")
	if slug == "" {
		slug = "heading"
	}
	return "h-" + slug
}

// collectFootnotes returns definition keys in document order plus their text.
// Order matters: the rendered list follows the order of definition.
func collectFootnotes(body string) ([]string, map[string]string) {
	var keys []string
	notes := map[string]string{}
	for _, line := range strings.Split(body, "\n") {
		m := footnoteDefRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if _, seen := notes[m[1]]; !seen {
			keys = append(keys, m[1])
		}
		notes[m[1]] = m[2]
	}
	return keys, notes
}

func footnotesHTML(keys []string, notes map[string]string, ctx *Context) string {
	var items strings.Builder
	for _, k := range keys {
		items.WriteString(`<li id="fn-` + escapeHTML(k) + `">` + inline(notes[k], ctx) +
			` <a class="fn-back" href="#fnref-` + escapeHTML(k) + `">↩</a></li>`)
	}
	return `<div class="footnotes"><hr><ol>` + items.String() + `</ol></div>`
}

// ------------------------------------------------------------- transclusion

func transclude(target string, ctx *Context) string {
	base := strings.TrimSpace(strings.SplitN(target, "#", 2)[0])
	label := escapeHTML(base)
	rel, ok := ctx.lookup(base)
	if !ok {
		return `<div class="embed embed-missing">![[` + label + `]] — not found</div>`
	}
	if ctx.depth >= MaxEmbedDepth || ctx.embedding[rel] {
		return `<div class="embed embed-cycle">` +
			`<a class="wikilink" href="` + escapeHTML(ctx.linkHref(rel)) + `">` + label + `</a>` +
			` (embed depth limit)</div>`
	}
	innerBody := ctx.NoteBody(rel)
	if innerBody == nil {
		return `<div class="embed embed-missing">![[` + label + `]] — unavailable</div>`
	}
	embedding := map[string]bool{rel: true}
	for k := range ctx.embedding {
		embedding[k] = true
	}
	sub := &Context{
		LinkMap: ctx.LinkMap, ImgSrc: ctx.ImgSrc, NoteBody: ctx.NoteBody,
		RunQuery: ctx.RunQuery, LinkHref: ctx.LinkHref,
		depth: ctx.depth + 1, embedding: embedding,
	}
	return `<div class="embed"><div class="embed-title">` +
		`<a class="wikilink" href="` + escapeHTML(ctx.linkHref(rel)) + `">` + label + `</a></div>` +
		RenderWith(*innerBody, sub) + `</div>`
}

// ------------------------------------------------------------- query blocks

func queryHTML(block string, ctx *Context) string {
	result := ctx.RunQuery(block)
	if result == nil {
		return `<div class="query query-error">query error: no runner</div>`
	}
	if len(result.Errors) > 0 {
		escaped := make([]string, len(result.Errors))
		for i, e := range result.Errors {
			escaped[i] = escapeHTML(e)
		}
		return `<div class="query query-error">query error: ` +
			strings.Join(escaped, "; ") + `</div>`
	}
	switch result.Render {
	case "count":
		return `<div class="query query-count">` + strconv.Itoa(len(result.Rows)) + `</div>`
	case "table":
		var head strings.Builder
		for _, c := range result.Columns {
			head.WriteString("<th>" + escapeHTML(c) + "</th>")
		}
		var trs strings.Builder
		for _, r := range result.Rows {
			trs.WriteString("<tr>")
			for _, c := range result.Columns {
				switch c {
				case "title":
					trs.WriteString(`<td><a class="wikilink" href="` +
						escapeHTML(ctx.linkHref(asString(r["path"]))) + `">` +
						escapeHTML(titleOrPath(r)) + `</a></td>`)
				case "tags":
					trs.WriteString("<td>")
					var tags []string
					if raw, ok := r["tags"].([]string); ok {
						tags = raw
					}
					parts := make([]string, len(tags))
					for i, t := range tags {
						parts[i] = `<span class="tag">#` + escapeHTML(t) + `</span>`
					}
					trs.WriteString(strings.Join(parts, " ") + "</td>")
				default:
					trs.WriteString("<td>" + escapeHTML(asString(r[c])) + "</td>")
				}
			}
			trs.WriteString("</tr>")
		}
		return `<div class="query table-wrap"><table><thead><tr>` + head.String() +
			`</tr></thead><tbody>` + trs.String() + `</tbody></table></div>`
	}
	var items strings.Builder
	for _, r := range result.Rows {
		items.WriteString(`<li><a class="wikilink" href="` +
			escapeHTML(ctx.linkHref(asString(r["path"]))) + `">` +
			escapeHTML(titleOrPath(r)) + `</a></li>`)
	}
	return `<div class="query"><ul>` + items.String() + `</ul></div>`
}

func titleOrPath(r map[string]any) string {
	if t := asString(r["title"]); t != "" {
		return t
	}
	return asString(r["path"])
}

func asString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// ------------------------------------------------------------------- tables

func isTableRow(line string) bool {
	s := strings.TrimSpace(line)
	return strings.HasPrefix(s, "|") && strings.Count(s, "|") >= 2
}

func isTableSep(line string) bool {
	s := strings.TrimSpace(line)
	return tableSepRE.MatchString(s) && strings.Contains(s, "-") && strings.Contains(s, "|")
}

func cells(line string) []string {
	trimmed := strings.Trim(strings.TrimSpace(line), "|")
	parts := strings.Split(trimmed, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func tableHTML(header string, rows []string, ctx *Context) string {
	var head strings.Builder
	for _, c := range cells(header) {
		head.WriteString("<th>" + inline(c, ctx) + "</th>")
	}
	var body strings.Builder
	for _, r := range rows {
		body.WriteString("<tr>")
		for _, c := range cells(r) {
			body.WriteString("<td>" + inline(c, ctx) + "</td>")
		}
		body.WriteString("</tr>")
	}
	return `<div class="table-wrap"><table><thead><tr>` + head.String() +
		`</tr></thead><tbody>` + body.String() + `</tbody></table></div>`
}

// -------------------------------------------------------------------- helpers

// escapeHTML matches Python's html.escape(quote=True) byte for byte. Go's
// html.EscapeString emits &#34;/&#39; where Python emits &quot;/&#x27;.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&quot;")
	return strings.ReplaceAll(s, "'", "&#x27;")
}

func isImage(target string) bool {
	t := strings.ToLower(strings.TrimSpace(target))
	for _, ext := range imageExts {
		if strings.HasSuffix(t, ext) {
			return true
		}
	}
	return false
}

func stripMD(path string) string {
	return strings.TrimSuffix(path, ".md")
}

// capitalize mirrors Python's str.capitalize: first character upper, rest lower.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return strings.ToUpper(string(r[0])) + strings.ToLower(string(r[1:]))
}
