package connectors

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// Turning other systems' document formats into markdown.
//
// The conversions here are deliberately shallow. A connector's job is to make
// the text retrievable and readable, not to reproduce a Confluence page's
// layout — and a faithful converter for Atlassian storage format is a project
// of its own. What matters for retrieval is that headings stay headings, lists
// stay lists, and no markup ends up in the embedding as if it were prose.

var (
	blockTagRE  = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|blockquote|pre)>`)
	headingRE   = regexp.MustCompile(`(?i)<h([1-6])[^>]*>`)
	listItemRE  = regexp.MustCompile(`(?i)<li[^>]*>`)
	breakRE     = regexp.MustCompile(`(?i)<br\s*/?>`)
	anyTagRE    = regexp.MustCompile(`(?s)<[^>]+>`)
	scriptRE    = regexp.MustCompile(`(?is)<(script|style)[^>]*>.*?</(script|style)>`)
	blankLineRE = regexp.MustCompile(`\n{3,}`)
)

// HTMLToMarkdown reduces HTML — Confluence storage format, an RSS entry, a web
// page — to markdown-ish plain text.
func HTMLToMarkdown(in string) string {
	if strings.TrimSpace(in) == "" {
		return ""
	}
	s := scriptRE.ReplaceAllString(in, "")
	s = headingRE.ReplaceAllStringFunc(s, func(m string) string {
		level := m[2] - '0'
		return "\n" + strings.Repeat("#", int(level)) + " "
	})
	s = listItemRE.ReplaceAllString(s, "\n- ")
	s = breakRE.ReplaceAllString(s, "\n")
	s = blockTagRE.ReplaceAllString(s, "\n\n")
	s = anyTagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// Collapse the runs of blank lines the tag stripping leaves behind.
	s = blankLineRE.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

// ADFToMarkdown flattens Atlassian Document Format — the JSON tree Jira uses
// for descriptions and comments — into markdown.
//
// Jira returns either ADF (API v3) or a plain string (v2 and some older
// instances), so both shapes are handled: a description that arrives as a
// string must not come out as an empty document.
func ADFToMarkdown(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return strings.TrimSpace(asString)
	}
	var node adfNode
	if err := json.Unmarshal(raw, &node); err != nil {
		return ""
	}
	var b strings.Builder
	writeADF(&b, node, 0)
	return strings.TrimSpace(blankLineRE.ReplaceAllString(b.String(), "\n\n"))
}

type adfNode struct {
	Type    string    `json:"type"`
	Text    string    `json:"text"`
	Content []adfNode `json:"content"`
	Attrs   struct {
		Level int    `json:"level"`
		URL   string `json:"url"`
		Text  string `json:"text"`
	} `json:"attrs"`
	Marks []struct {
		Type  string `json:"type"`
		Attrs struct {
			Href string `json:"href"`
		} `json:"attrs"`
	} `json:"marks"`
}

func writeADF(b *strings.Builder, n adfNode, depth int) {
	switch n.Type {
	case "text":
		text := n.Text
		for _, m := range n.Marks {
			switch m.Type {
			case "code":
				text = "`" + text + "`"
			case "strong":
				text = "**" + text + "**"
			case "em":
				text = "*" + text + "*"
			case "link":
				if m.Attrs.Href != "" {
					text = "[" + text + "](" + m.Attrs.Href + ")"
				}
			}
		}
		b.WriteString(text)
		return
	case "hardBreak":
		b.WriteString("\n")
		return
	case "heading":
		level := n.Attrs.Level
		if level < 1 || level > 6 {
			level = 2
		}
		b.WriteString("\n" + strings.Repeat("#", level) + " ")
	case "paragraph":
		b.WriteString("\n\n")
	case "listItem":
		b.WriteString("\n" + strings.Repeat("  ", depth) + "- ")
	case "codeBlock":
		b.WriteString("\n\n```\n")
		for _, c := range n.Content {
			writeADF(b, c, depth)
		}
		b.WriteString("\n```\n\n")
		return
	case "rule":
		b.WriteString("\n\n---\n\n")
		return
	case "mention":
		b.WriteString(n.Attrs.Text)
		return
	case "inlineCard":
		if n.Attrs.URL != "" {
			b.WriteString(n.Attrs.URL)
		}
		return
	}
	next := depth
	if n.Type == "bulletList" || n.Type == "orderedList" {
		next = depth + 1
	}
	for _, c := range n.Content {
		writeADF(b, c, next)
	}
}
