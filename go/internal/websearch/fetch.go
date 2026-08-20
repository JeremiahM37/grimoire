package websearch

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Reading a page, not just finding it.
//
// A search result is a title and two lines; the answer is in the page. This is
// the fetch half — the same pair every assistant ends up with — and it is the
// half that needs guarding, because the URL comes from whoever is asking.

var (
	tagRE      = regexp.MustCompile(`(?s)<[^>]+>`)
	dropRE     = regexp.MustCompile(`(?is)<(script|style|nav|footer|header|form|noscript|svg)[^>]*>.*?</(script|style|nav|footer|header|form|noscript|svg)>`)
	titleRE    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	blockRE    = regexp.MustCompile(`(?i)</(p|div|h[1-6]|li|tr|section|article|blockquote)>`)
	spacesRE   = regexp.MustCompile(`[ \t]{2,}`)
	newlinesRE = regexp.MustCompile(`\n{3,}`)
)

// maxPage bounds one fetched document. Beyond this it is a download, not a
// page worth reading into a model's context.
const maxPage = 4 << 20

// Fetch retrieves URLs and returns their readable text.
//
// One failure does not fail the batch: an agent asking for five pages should
// get the four that worked, with the fifth explaining itself.
func (c *Client) Fetch(ctx context.Context, urls []string, maxChars int) []Page {
	if maxChars <= 0 {
		maxChars = 20000
	}
	out := make([]Page, len(urls))
	var wg sync.WaitGroup
	// Bounded concurrency: a batch of twenty URLs should not open twenty
	// sockets at once from a self-hosted box.
	sem := make(chan struct{}, 4)
	for i, u := range urls {
		wg.Add(1)
		go func(i int, u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			page, err := c.fetchOne(ctx, u, maxChars)
			if err != nil {
				out[i] = Page{URL: u, Error: err.Error()}
				return
			}
			out[i] = page
		}(i, u)
	}
	wg.Wait()
	return out
}

func (c *Client) fetchOne(ctx context.Context, raw string, maxChars int) (Page, error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "https://" + raw
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return Page{}, err
	}
	// Some sites serve a different (or no) page to an unrecognized agent. This
	// says what it is rather than pretending to be a browser.
	req.Header.Set("User-Agent", "grimoire (+https://github.com/JeremiahM37/grimoire)")
	req.Header.Set("Accept", "text/html,text/plain,application/xhtml+xml")

	client := c.client()
	if client.Timeout == 0 {
		client.Timeout = 30 * time.Second
	}
	resp, err := client.Do(req)
	if err != nil {
		return Page{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return Page{}, fmt.Errorf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	ctype := resp.Header.Get("Content-Type")
	if ct := strings.ToLower(ctype); ct != "" &&
		!strings.Contains(ct, "html") && !strings.Contains(ct, "text") &&
		!strings.Contains(ct, "json") && !strings.Contains(ct, "xml") {
		return Page{}, fmt.Errorf("not a readable document (%s)", ctype)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPage))
	if err != nil {
		return Page{}, err
	}

	page := Page{URL: resp.Request.URL.String()}
	if m := titleRE.FindSubmatch(body); len(m) == 2 {
		page.Title = strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(string(m[1]), "")))
	}
	page.Text = Readable(string(body))
	if len([]rune(page.Text)) > maxChars {
		page.Text = string([]rune(page.Text)[:maxChars]) + "\n\n…(truncated)"
	}
	return page, nil
}

// Readable reduces an HTML document to the text a reader would keep.
//
// Deliberately crude: navigation, scripts and styles go, block tags become
// paragraph breaks, everything else becomes text. A real extractor (readability,
// boilerplate removal) is a project of its own, and for feeding a model the
// difference between "the article" and "the article plus the footer" is far
// smaller than the difference between text and a page of tags.
func Readable(doc string) string {
	s := dropRE.ReplaceAllString(doc, " ")
	s = blockRE.ReplaceAllString(s, "\n\n")
	s = regexp.MustCompile(`(?i)<br\s*/?>`).ReplaceAllString(s, "\n")
	s = tagRE.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = spacesRE.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(newlinesRE.ReplaceAllString(s, "\n\n"))
}

// stripTags removes markup from a provider's snippet, which several of them
// return with <strong> around the matched terms.
func stripTags(s string) string {
	return strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(s, "")))
}
