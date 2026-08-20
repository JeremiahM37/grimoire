package render

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

// Live template blocks — a dynamic page part.
//
// A template inserted by a slash command is a one-time copy: it is expanded at
// the moment you type it and never again, so a "weekly review" template that
// lists this week's open tasks is right on Monday and wrong by Wednesday. What
// was missing was a template that renders WHERE IT SITS, every time the page is
// read.
//
//	```template
//	use: weekly-review
//	owner: ana
//	```
//
// The template's body is rendered as markdown, which is what makes it a widget
// rather than a snippet: a query block inside it runs, a transclusion inside it
// resolves, and the whole thing goes through the same depth and cycle guards
// as `![[embed]]` — a template that uses itself is caught rather than looping.
//
// Substitution is textual and deliberately dumb: {{name}} becomes the value
// given in the fence, or a built-in date. There is no expression language here
// for the same reason there is none in a query block's formulas.

// TemplatePrefix is where a named template lives.
const TemplatePrefix = "templates/"

var templateVarRE = regexp.MustCompile(`\{\{([a-zA-Z][a-zA-Z0-9_-]{0,63})\}\}`)

// templateNameRE is what may name a template. A subdirectory is allowed; ".."
// and anything else that could climb out of templates/ is not.
var templateNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _-]*(?:/[A-Za-z0-9][A-Za-z0-9 _-]*)*$`)

// Now is the clock template dates render against. A variable so a test can pin
// it — a page that renders today's date is otherwise untestable.
var Now = func() time.Time { return time.Now() }

// templateHTML renders one ```template block.
func templateHTML(block string, ctx *Context) string {
	name, vars := parseTemplateBlock(block)
	if name == "" {
		return `<div class="template template-error">template error: ` +
			`needs 'use: name'</div>`
	}
	if ctx.TemplateBody == nil {
		return `<div class="template template-error">template error: ` +
			`templates are not available here</div>`
	}
	if !templateNameRE.MatchString(name) {
		// The name becomes a path under templates/, so it may not climb out
		// of it. A template block must not be a way to read an arbitrary
		// file under a friendlier name than ![[embed]] would give it.
		return `<div class="template template-missing">template '` +
			escapeHTML(name) + `' not found</div>`
	}
	rel := TemplatePrefix + name + ".md"
	if ctx.depth >= MaxEmbedDepth || ctx.embedding[rel] {
		return `<div class="template template-cycle">template '` +
			escapeHTML(name) + `' (depth limit)</div>`
	}
	body := ctx.TemplateBody(name)
	if body == nil {
		return `<div class="template template-missing">template '` +
			escapeHTML(name) + `' not found</div>`
	}
	embedding := map[string]bool{rel: true}
	for k := range ctx.embedding {
		embedding[k] = true
	}
	sub := &Context{
		LinkMap: ctx.LinkMap, ImgSrc: ctx.ImgSrc, NoteBody: ctx.NoteBody,
		TemplateBody: ctx.TemplateBody, RunQuery: ctx.RunQuery,
		LinkHref: ctx.LinkHref,
		depth:    ctx.depth + 1, embedding: embedding,
	}
	return `<div class="template" data-template="` + escapeHTML(name) + `">` +
		RenderWith(ExpandVars(*body, vars, Now()), sub) + `</div>`
}

// parseTemplateBlock reads the fence: `use:` names the template, every other
// line is a variable.
func parseTemplateBlock(block string) (name string, vars map[string]string) {
	vars = map[string]string{}
	for _, raw := range strings.Split(block, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		if key == "use" || key == "template" {
			name = strings.TrimSuffix(val, ".md")
			continue
		}
		vars[key] = val
	}
	return name, vars
}

// ExpandVars substitutes {{name}} placeholders.
//
// A placeholder with no value is left ALONE rather than blanked. A template
// carrying an unfilled slot should look unfilled — blanking it produces a page
// that reads as finished and is missing something, which is the failure nobody
// notices.
func ExpandVars(body string, vars map[string]string, now time.Time) string {
	return templateVarRE.ReplaceAllStringFunc(body, func(match string) string {
		name := strings.ToLower(match[2 : len(match)-2])
		if v, ok := vars[name]; ok {
			return v
		}
		if v, ok := builtinVar(name, now); ok {
			return v
		}
		return match
	})
}

// builtinVar covers the dates a template almost always wants, so a weekly note
// does not need every one passed in.
func builtinVar(name string, now time.Time) (string, bool) {
	switch name {
	case "date", "today":
		return now.Format("2006-01-02"), true
	case "time":
		return now.Format("15:04"), true
	case "datetime":
		return now.Format("2006-01-02 15:04"), true
	case "year":
		return now.Format("2006"), true
	case "month":
		return now.Format("01"), true
	case "day":
		return now.Format("02"), true
	case "weekday":
		return now.Format("Monday"), true
	case "yesterday":
		return now.AddDate(0, 0, -1).Format("2006-01-02"), true
	case "tomorrow":
		return now.AddDate(0, 0, 1).Format("2006-01-02"), true
	}
	return "", false
}

// TemplateVars lists the placeholders a template body uses, so a caller can
// ask for them rather than making someone read the template to find out.
func TemplateVars(body string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range templateVarRE.FindAllStringSubmatch(body, -1) {
		name := strings.ToLower(m[1])
		if seen[name] {
			continue
		}
		if _, builtin := builtinVar(name, time.Time{}); builtin {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
