// Package websearch answers questions the vault cannot.
//
// An agent working from notes alone is stuck the moment the answer is not in
// them — a library's current API, an error message nobody has written down, what
// happened last week. Onyx, Claude and every other assistant solve this the same
// way: a search tool and a fetch tool. This is that, self-hosted.
//
// Four providers, because there is no single right answer for a self-hoster:
// SearXNG for people who already run one and want no third party at all, and
// Brave, Serper or a Google Programmable Search key for people who would rather
// have someone else's index. The provider is configuration, not a code change.
//
// Fetching is the part to be careful about. The URL comes from whoever is
// asking — an agent, ultimately a model — so it goes through the same outbound
// guard the credential broker uses: no private ranges, no cloud metadata, and
// the check is on the address the socket is about to use, so a DNS name that
// resolves publicly once and privately on the retry is still refused.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Result is one search hit.
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Source  string `json:"source,omitempty"`
}

// Page is a fetched document, reduced to text.
type Page struct {
	URL   string `json:"url"`
	Title string `json:"title"`
	Text  string `json:"text"`
	Error string `json:"error,omitempty"`
}

// Settings is the configuration a deployment supplies. Read from the settings
// store, so it can change without a restart.
type Settings interface {
	Get(key string) string
}

// Secrets resolves an API key held in the credential vault, so a search key
// does not have to sit in a settings file.
type Secrets func(name string) (string, error)

// Client performs searches and fetches.
type Client struct {
	Settings Settings
	Secrets  Secrets
	HTTP     *http.Client
}

var (
	// ErrNotConfigured is returned rather than an empty result set: "no
	// provider configured" and "the web has nothing" are different answers,
	// and an agent that cannot tell them apart will report the second.
	ErrNotConfigured = errors.New("web search is not configured — set web_search_provider " +
		"(searxng, brave, serper or google) in settings")
	ErrNoKey = errors.New("this web-search provider needs an API key")
)

// Provider reports which backend is configured, if any.
func (c *Client) Provider() string {
	if c == nil || c.Settings == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(c.Settings.Get("web_search_provider")))
}

// Available reports whether searching will work at all.
func (c *Client) Available() bool { return c.Provider() != "" }

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// key resolves the provider's API key: from the vault when the setting names a
// credential, otherwise from the setting itself.
func (c *Client) key() (string, error) {
	name := strings.TrimSpace(c.Settings.Get("web_search_key"))
	if name == "" {
		return "", ErrNoKey
	}
	if strings.HasPrefix(name, "vault:") && c.Secrets != nil {
		value, err := c.Secrets(strings.TrimPrefix(name, "vault:"))
		if err != nil {
			return "", fmt.Errorf("web-search key %q: %w (is the vault unlocked?)",
				strings.TrimPrefix(name, "vault:"), err)
		}
		return value, nil
	}
	return name, nil
}

// Search runs a query against the configured provider.
func (c *Client) Search(ctx context.Context, query string, n int) ([]Result, error) {
	if strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if n <= 0 || n > 25 {
		n = 5
	}
	switch c.Provider() {
	case "searxng":
		return c.searxng(ctx, query, n)
	case "brave":
		return c.brave(ctx, query, n)
	case "serper":
		return c.serper(ctx, query, n)
	case "google", "pse":
		return c.googlePSE(ctx, query, n)
	case "":
		return nil, ErrNotConfigured
	default:
		return nil, fmt.Errorf("unknown web search provider %q", c.Provider())
	}
}

func (c *Client) searxng(ctx context.Context, query string, n int) ([]Result, error) {
	base := strings.TrimRight(strings.TrimSpace(c.Settings.Get("web_search_url")), "/")
	if base == "" {
		return nil, fmt.Errorf("searxng needs web_search_url (e.g. http://searxng:8080)")
	}
	u := base + "/search?" + url.Values{
		"q": {query}, "format": {"json"}, "safesearch": {"1"},
	}.Encode()
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
			Engine  string `json:"engine"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, u, nil, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, n)
	for _, r := range out.Results {
		if len(results) >= n {
			break
		}
		results = append(results, Result{Title: r.Title, URL: r.URL,
			Snippet: r.Content, Source: r.Engine})
	}
	return results, nil
}

func (c *Client) brave(ctx context.Context, query string, n int) ([]Result, error) {
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	u := "https://api.search.brave.com/res/v1/web/search?" + url.Values{
		"q": {query}, "count": {strconv.Itoa(n)},
	}.Encode()
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := c.getJSON(ctx, u, map[string]string{
		"X-Subscription-Token": key, "Accept": "application/json"}, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Web.Results))
	for _, r := range out.Web.Results {
		results = append(results, Result{Title: r.Title, URL: r.URL,
			Snippet: stripTags(r.Description), Source: "brave"})
	}
	return results, nil
}

func (c *Client) serper(ctx context.Context, query string, n int) ([]Result, error) {
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(map[string]any{"q": query, "num": n})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://google.serper.dev/search", strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-KEY", key)
	req.Header.Set("Content-Type", "application/json")
	var out struct {
		Organic []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"organic"`
	}
	if err := c.do(req, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Organic))
	for _, r := range out.Organic {
		results = append(results, Result{Title: r.Title, URL: r.Link,
			Snippet: r.Snippet, Source: "serper"})
	}
	return results, nil
}

func (c *Client) googlePSE(ctx context.Context, query string, n int) ([]Result, error) {
	key, err := c.key()
	if err != nil {
		return nil, err
	}
	cx := strings.TrimSpace(c.Settings.Get("web_search_cx"))
	if cx == "" {
		return nil, fmt.Errorf("google programmable search needs web_search_cx " +
			"(the search engine id from programmablesearchengine.google.com)")
	}
	u := "https://www.googleapis.com/customsearch/v1?" + url.Values{
		"key": {key}, "cx": {cx}, "q": {query}, "num": {strconv.Itoa(min(n, 10))},
	}.Encode()
	var out struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := c.getJSON(ctx, u, nil, &out); err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(out.Items))
	for _, r := range out.Items {
		results = append(results, Result{Title: r.Title, URL: r.Link,
			Snippet: r.Snippet, Source: "google"})
	}
	return results, nil
}

func (c *Client) getJSON(ctx context.Context, rawURL string, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grimoire")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	resp, err := c.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return fmt.Errorf("%s: %d: %s", req.URL.Host, resp.StatusCode, snippet)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}
