package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Notion: pages and database rows as markdown.
//
// Notion is where a great many teams keep the document Grimoire is supposed to
// be able to answer from, and it is the single most common thing people ask a
// knowledge tool to read. Its content model is a tree of typed blocks rather
// than text, so the work here is the conversion — a page that arrives as JSON
// nobody can grep is not knowledge that outlives the app.

func init() { Register(notion{}) }

type notion struct{}

func (notion) Kind() string { return "notion" }

func (notion) Describe() Kind {
	return Kind{
		Kind: "notion",
		Name: "Notion",
		Help: "Pulls pages you have shared with an integration, converted to markdown. " +
			"Create an internal integration at notion.so/my-integrations, then open each " +
			"page or database you want indexed and use ••• → Connections → your " +
			"integration. Nothing is readable until you share it that way.",
		SecretHelp: "The Internal Integration Secret from notion.so/my-integrations " +
			"(starts with ntn_ or secret_).",
		Fields: []Field{
			{Name: "query", Label: "Restrict to titles containing",
				Help: "Optional. Empty pulls everything shared with the integration."},
		},
		DefaultPrefix: "connectors/notion",
	}
}

// notionSearch is the search response: pages, with their properties.
type notionSearch struct {
	Results []struct {
		ID             string          `json:"id"`
		URL            string          `json:"url"`
		LastEditedTime string          `json:"last_edited_time"`
		Object         string          `json:"object"`
		Properties     json.RawMessage `json:"properties"`
	} `json:"results"`
	NextCursor string `json:"next_cursor"`
	HasMore    bool   `json:"has_more"`
}

const notionVersion = "2022-06-28"

func notionHeaders(secret string) map[string]string {
	return map[string]string{
		"Authorization":  "Bearer " + secret,
		"Notion-Version": notionVersion,
		"Content-Type":   "application/json",
	}
}

func (n notion) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, fmt.Errorf("%w: an integration secret is required", ErrConfig)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	// Search is a POST with a JSON body, so this cannot use jsonRequest.
	body := map[string]any{
		"page_size": limit,
		"filter":    map[string]string{"property": "object", "value": "page"},
		"sort":      map[string]string{"direction": "ascending", "timestamp": "last_edited_time"},
	}
	if q := in.Config.Get("query"); q != "" {
		body["query"] = q
	}
	if in.Cursor != "" {
		body["start_cursor"] = in.Cursor
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.notion.com/v1/search", bytes.NewReader(raw))
	if err != nil {
		return Page{}, err
	}
	for k, v := range notionHeaders(in.Secret) {
		req.Header.Set(k, v)
	}
	var out notionSearch
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}

	docs := make([]Document, 0, len(out.Results))
	for _, r := range out.Results {
		if r.Object != "page" {
			continue
		}
		title := notionTitle(r.Properties)
		if title == "" {
			title = "Untitled"
		}
		md, err := notionBlocks(ctx, in, r.ID, 0)
		if err != nil {
			// One unreadable page must not fail the whole sync — it is usually
			// a permission the operator has not granted yet, and the rest of
			// the pull is still worth having.
			md = "_(could not read page body: " + err.Error() + ")_"
		}
		docs = append(docs, Document{
			ExternalID: r.ID,
			Title:      title,
			Body:       md,
			URL:        r.URL,
			Updated:    r.LastEditedTime,
			Meta:       map[string]string{"source": "notion"},
		})
	}
	return Page{Docs: docs, Cursor: out.NextCursor, More: out.HasMore}, nil
}

// notionTitle digs the page title out of the properties blob.
//
// Notion has no fixed title field: the title lives in whichever property has
// type "title", and its name is whatever the database calls it. So this looks
// for the type rather than a name.
func notionTitle(props json.RawMessage) string {
	if len(props) == 0 {
		return ""
	}
	var m map[string]struct {
		Type  string `json:"type"`
		Title []struct {
			PlainText string `json:"plain_text"`
		} `json:"title"`
	}
	if err := json.Unmarshal(props, &m); err != nil {
		return ""
	}
	for _, p := range m {
		if p.Type != "title" {
			continue
		}
		var b strings.Builder
		for _, t := range p.Title {
			b.WriteString(t.PlainText)
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

// notionMaxDepth bounds recursion into child blocks. Notion nests without
// limit, and a toggle containing a toggle containing a database is a real page
// somebody has; this stops one of them turning a sync into a crawl.
const notionMaxDepth = 3

// notionBlocks renders a page's blocks as markdown.
func notionBlocks(ctx context.Context, in Input, pageID string, depth int) (string, error) {
	if depth > notionMaxDepth {
		return "", nil
	}
	req, err := jsonRequest("https://api.notion.com/v1/blocks/"+pageID+"/children",
		nil, notionHeaders(in.Secret))
	if err != nil {
		return "", err
	}
	// Decoded loosely: the block schema is one shape per type and a struct per
	// type would be forty structs that break whenever Notion adds one.
	var out struct {
		Results []map[string]any `json:"results"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, blk := range out.Results {
		b.WriteString(notionBlockMarkdown(blk))
	}
	return strings.TrimSpace(b.String()), nil
}

// notionBlockMarkdown converts one block. Unknown types degrade to their plain
// text rather than vanishing, because losing a paragraph silently is worse than
// rendering it without its formatting.
func notionBlockMarkdown(blk map[string]any) string {
	typ, _ := blk["type"].(string)
	inner, _ := blk[typ].(map[string]any)
	text := notionRichText(inner)

	switch typ {
	case "heading_1":
		return "# " + text + "\n\n"
	case "heading_2":
		return "## " + text + "\n\n"
	case "heading_3":
		return "### " + text + "\n\n"
	case "bulleted_list_item":
		return "- " + text + "\n"
	case "numbered_list_item":
		return "1. " + text + "\n"
	case "to_do":
		done, _ := inner["checked"].(bool)
		box := "[ ]"
		if done {
			box = "[x]"
		}
		return "- " + box + " " + text + "\n"
	case "quote":
		return "> " + text + "\n\n"
	case "code":
		lang, _ := inner["language"].(string)
		return "```" + lang + "\n" + text + "\n```\n\n"
	case "divider":
		return "---\n\n"
	case "callout":
		return "> " + text + "\n\n"
	case "toggle":
		return "- " + text + "\n"
	case "child_page", "child_database":
		if t, _ := inner["title"].(string); t != "" {
			return "- [[" + t + "]]\n"
		}
		return ""
	case "image", "file", "video", "pdf":
		if text != "" {
			return "_(" + typ + ": " + text + ")_\n\n"
		}
		return "_(" + typ + ")_\n\n"
	default:
		if text == "" {
			return ""
		}
		return text + "\n\n"
	}
}

// notionRichText flattens a rich_text array, keeping the marks markdown has.
func notionRichText(inner map[string]any) string {
	if inner == nil {
		return ""
	}
	arr, ok := inner["rich_text"].([]any)
	if !ok {
		if c, ok := inner["caption"].([]any); ok {
			arr = c
		} else {
			return ""
		}
	}
	var b strings.Builder
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		txt, _ := m["plain_text"].(string)
		if txt == "" {
			continue
		}
		if ann, ok := m["annotations"].(map[string]any); ok {
			if v, _ := ann["code"].(bool); v {
				txt = "`" + txt + "`"
			}
			if v, _ := ann["bold"].(bool); v {
				txt = "**" + txt + "**"
			}
			if v, _ := ann["italic"].(bool); v {
				txt = "*" + txt + "*"
			}
			if v, _ := ann["strikethrough"].(bool); v {
				txt = "~~" + txt + "~~"
			}
		}
		if href, _ := m["href"].(string); href != "" {
			txt = "[" + txt + "](" + href + ")"
		}
		b.WriteString(txt)
	}
	return strings.TrimSpace(b.String())
}
