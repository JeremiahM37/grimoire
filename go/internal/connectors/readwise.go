package connectors

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Readwise: highlights from books, articles and papers, grouped by source.
//
// This is the one connector aimed squarely at the person rather than the team.
// Readwise is where a large part of the Obsidian and note-taking world already
// keeps everything it has ever underlined, and a highlight is the highest-value
// text per byte a knowledge base can hold: it is already the part somebody
// decided was worth keeping.
//
// One document per source, not per highlight — the same reasoning as Discord's
// day grouping. A lone sentence with no context retrieves badly and buries a
// vault; a book's highlights together read like notes on that book.

func init() { Register(readwise{}) }

type readwise struct{}

func (readwise) Kind() string { return "readwise" }

func (readwise) Describe() Kind {
	return Kind{
		Kind: "readwise",
		Name: "Readwise",
		Help: "Pulls your highlights, one document per book or article, with the notes " +
			"you attached to them.",
		SecretHelp: "Your access token from readwise.io/access_token.",
		Fields: []Field{
			{Name: "category", Label: "Only this category", Placeholder: "books",
				Help: "Optional: books, articles, tweets, supplementals or podcasts."},
		},
		DefaultPrefix: "connectors/readwise",
	}
}

type readwiseExport struct {
	Count          int `json:"count"`
	NextPageCursor any `json:"nextPageCursor"`
	Results        []struct {
		UserBookID  int    `json:"user_book_id"`
		Title       string `json:"title"`
		Author      string `json:"author"`
		Category    string `json:"category"`
		SourceURL   string `json:"source_url"`
		ReadwiseURL string `json:"readwise_url"`
		CoverImage  string `json:"cover_image_url"`
		Highlights  []struct {
			ID            int    `json:"id"`
			Text          string `json:"text"`
			Note          string `json:"note"`
			Location      int    `json:"location"`
			HighlightedAt string `json:"highlighted_at"`
			UpdatedAt     string `json:"updated"`
			URL           string `json:"url"`
			Tags          []struct {
				Name string `json:"name"`
			} `json:"tags"`
		} `json:"highlights"`
	} `json:"results"`
}

func (r readwise) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, fmt.Errorf("%w: an access token is required", ErrConfig)
	}
	q := url.Values{}
	// updatedAfter is what makes this incremental: the export endpoint returns
	// only books whose highlights changed since then, so a re-sync is cheap.
	if in.Cursor != "" && !strings.HasPrefix(in.Cursor, "page:") {
		q.Set("updatedAfter", in.Cursor)
	}
	if p := strings.TrimPrefix(in.Cursor, "page:"); p != in.Cursor && p != "" {
		q.Set("pageCursor", p)
	}
	req, err := jsonRequest("https://readwise.io/api/v2/export/", q,
		map[string]string{"Authorization": "Token " + in.Secret})
	if err != nil {
		return Page{}, err
	}
	var out readwiseExport
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}

	want := strings.ToLower(in.Config.Get("category"))
	docs := make([]Document, 0, len(out.Results))
	var newest string
	for _, bk := range out.Results {
		if want != "" && strings.ToLower(bk.Category) != want {
			continue
		}
		if len(bk.Highlights) == 0 {
			continue
		}
		var b strings.Builder
		if bk.Author != "" {
			fmt.Fprintf(&b, "**%s**\n\n", bk.Author)
		}
		if bk.SourceURL != "" {
			fmt.Fprintf(&b, "[Source](%s)\n\n", bk.SourceURL)
		}
		for _, h := range bk.Highlights {
			text := strings.TrimSpace(h.Text)
			if text == "" {
				continue
			}
			// Blockquote, because a highlight is somebody else's words. That
			// distinction matters here more than in most tools: the trust model
			// reads quoted source text differently from the operator's own.
			for _, line := range strings.Split(text, "\n") {
				fmt.Fprintf(&b, "> %s\n", line)
			}
			var tags []string
			for _, t := range h.Tags {
				tags = append(tags, "#"+strings.ReplaceAll(t.Name, " ", "-"))
			}
			if len(tags) > 0 {
				fmt.Fprintf(&b, ">\n> %s\n", strings.Join(tags, " "))
			}
			b.WriteString("\n")
			if note := strings.TrimSpace(h.Note); note != "" {
				fmt.Fprintf(&b, "%s\n\n", note)
			}
			if h.UpdatedAt > newest {
				newest = h.UpdatedAt
			}
		}
		body := strings.TrimSpace(b.String())
		if body == "" {
			continue
		}
		docs = append(docs, Document{
			ExternalID: strconv.Itoa(bk.UserBookID),
			Title:      bk.Title,
			Body:       body,
			URL:        firstNonEmpty(bk.SourceURL, bk.ReadwiseURL),
			Updated:    newest,
			Author:     bk.Author,
			Meta: map[string]string{
				"category":   bk.Category,
				"highlights": strconv.Itoa(len(bk.Highlights)),
			},
		})
	}

	// The cursor is either "more pages of this run" or "resume from this time".
	// Prefixing distinguishes them, because they are read differently above.
	cursor := in.Cursor
	more := false
	if s, ok := out.NextPageCursor.(string); ok && s != "" {
		cursor, more = "page:"+s, true
	} else if f, ok := out.NextPageCursor.(float64); ok && f != 0 {
		cursor, more = "page:"+strconv.Itoa(int(f)), true
	} else if newest != "" {
		cursor = newest
	}
	return Page{Docs: docs, Cursor: cursor, More: more}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
