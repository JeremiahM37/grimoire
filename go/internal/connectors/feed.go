package connectors

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// RSS and Atom, which is how everything without an API is still readable:
// status pages, changelogs, blogs, a search that emits a feed.
//
// No credential, so this is also the connector to reach for when testing that
// the plumbing works at all.

func init() { Register(feed{}) }

type feed struct{}

func (feed) Kind() string { return "rss" }

func (feed) Describe() Kind {
	return Kind{
		Kind: "rss",
		Name: "RSS / Atom feed",
		Help: "Pulls entries from any feed — a changelog, a status page, a blog. " +
			"No credential needed.",
		Fields: []Field{
			{Name: "url", Label: "Feed URL", Required: true,
				Placeholder: "https://example.com/feed.xml"},
		},
		DefaultPrefix: "connectors/feeds",
	}
}

type feedDoc struct {
	Channel struct {
		Title string `xml:"title"`
		Items []struct {
			Title       string `xml:"title"`
			Link        string `xml:"link"`
			GUID        string `xml:"guid"`
			PubDate     string `xml:"pubDate"`
			Description string `xml:"description"`
			Encoded     string `xml:"encoded"`
			Author      string `xml:"creator"`
		} `xml:"item"`
	} `xml:"channel"`
	// Atom
	Title   string `xml:"title"`
	Entries []struct {
		Title   string `xml:"title"`
		ID      string `xml:"id"`
		Updated string `xml:"updated"`
		Content string `xml:"content"`
		Summary string `xml:"summary"`
		Author  struct {
			Name string `xml:"name"`
		} `xml:"author"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (f feed) Fetch(ctx context.Context, in Input) (Page, error) {
	src := in.Config.Get("url")
	if src == "" {
		return Page{}, missing("a feed URL")
	}
	req, err := jsonRequest(src, nil, map[string]string{"Accept": "application/rss+xml, application/atom+xml, application/xml, text/xml"})
	if err != nil {
		return Page{}, err
	}
	client := in.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return Page{}, err
	}
	if resp.StatusCode >= 400 {
		return Page{}, statusError(req, resp.StatusCode, body)
	}

	var doc feedDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return Page{}, fmt.Errorf("%s: not a readable feed: %w", src, err)
	}

	page := Page{Cursor: in.Cursor}
	source := doc.Channel.Title
	if source == "" {
		source = doc.Title
	}
	for _, it := range doc.Channel.Items {
		id := it.GUID
		if id == "" {
			id = it.Link
		}
		content := it.Encoded
		if content == "" {
			content = it.Description
		}
		page.Docs = append(page.Docs, Document{
			ExternalID: id,
			Title:      strings.TrimSpace(it.Title),
			Body:       HTMLToMarkdown(content),
			URL:        it.Link,
			Updated:    feedTime(it.PubDate),
			Author:     it.Author,
			Meta:       map[string]string{"feed": source, "source": "rss"},
		})
	}
	for _, e := range doc.Entries {
		link := ""
		for _, l := range e.Links {
			if l.Rel == "" || l.Rel == "alternate" {
				link = l.Href
				break
			}
		}
		content := e.Content
		if content == "" {
			content = e.Summary
		}
		page.Docs = append(page.Docs, Document{
			ExternalID: e.ID,
			Title:      strings.TrimSpace(e.Title),
			Body:       HTMLToMarkdown(content),
			URL:        link,
			Updated:    feedTime(e.Updated),
			Author:     e.Author.Name,
			Meta:       map[string]string{"feed": source, "source": "atom"},
		})
	}
	for _, d := range page.Docs {
		if d.Updated > page.Cursor {
			page.Cursor = d.Updated
		}
	}
	return page, nil
}

// feedTime accepts the several date formats feeds use in practice.
func feedTime(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC3339, time.RFC822Z, time.RFC822,
		"2006-01-02T15:04:05Z0700", "2006-01-02 15:04:05", "2006-01-02",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return rfc3339(t)
		}
	}
	return s
}
