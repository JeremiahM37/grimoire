package websearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type settings map[string]string

func (s settings) Get(k string) string { return s[k] }

func TestSearxngResultsAreMapped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("format") != "json" {
			t.Errorf("format = %q", r.URL.Query().Get("format"))
		}
		fmt.Fprint(w, `{"results":[
          {"title":"Go maps","url":"https://go.dev/blog/maps","content":"How maps work","engine":"duckduckgo"},
          {"title":"Second","url":"https://x/2","content":"…","engine":"brave"}]}`)
	}))
	defer srv.Close()

	c := &Client{Settings: settings{"web_search_provider": "searxng", "web_search_url": srv.URL},
		HTTP: srv.Client()}
	got, err := c.Search(context.Background(), "how do go maps work", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("n was not honoured: %d results", len(got))
	}
	if got[0].URL != "https://go.dev/blog/maps" || got[0].Source != "duckduckgo" {
		t.Fatalf("result = %+v", got[0])
	}
}

// "No provider configured" and "the web has nothing" are different answers,
// and an agent that cannot tell them apart reports the second.
func TestUnconfiguredSearchSaysSoRatherThanReturningNothing(t *testing.T) {
	c := &Client{Settings: settings{}}
	if c.Available() {
		t.Fatal("an unconfigured client reports itself available")
	}
	_, err := c.Search(context.Background(), "anything", 5)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

func TestProvidersNeedingKeysSayWhichSettingIsMissing(t *testing.T) {
	for _, provider := range []string{"brave", "serper", "google"} {
		c := &Client{Settings: settings{"web_search_provider": provider}}
		if _, err := c.Search(context.Background(), "q", 3); err == nil {
			t.Errorf("%s searched with no key", provider)
		}
	}
	// google also needs the engine id, and says so separately
	c := &Client{Settings: settings{"web_search_provider": "google", "web_search_key": "k"}}
	_, err := c.Search(context.Background(), "q", 3)
	if err == nil || !strings.Contains(err.Error(), "web_search_cx") {
		t.Fatalf("google without cx: %v", err)
	}
}

// A key may name a vault credential instead of being one, so a search key does
// not have to sit in a settings file.
func TestKeysCanComeFromTheVault(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Subscription-Token")
		fmt.Fprint(w, `{"web":{"results":[{"title":"t","url":"https://x","description":"<strong>d</strong>"}]}}`)
	}))
	defer srv.Close()

	c := &Client{
		Settings: settings{"web_search_provider": "brave", "web_search_key": "vault:brave-key"},
		Secrets:  func(name string) (string, error) { return "resolved-" + name, nil },
		HTTP:     srv.Client(),
	}
	c.HTTP.Transport = rewrite{srv.URL}
	got, err := c.Search(context.Background(), "q", 3)
	if err != nil {
		t.Fatal(err)
	}
	if gotKey != "resolved-brave-key" {
		t.Fatalf("key sent = %q", gotKey)
	}
	if got[0].Snippet != "d" {
		t.Errorf("provider markup survived: %q", got[0].Snippet)
	}

	// A locked vault must say so, not fall back to searching unauthenticated.
	c.Secrets = func(string) (string, error) { return "", errors.New("vault is locked") }
	if _, err := c.Search(context.Background(), "q", 3); err == nil ||
		!strings.Contains(err.Error(), "vault") {
		t.Fatalf("locked vault: %v", err)
	}
}

func TestFetchExtractsReadableText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><head><title>Rollback guide</title>
          <style>body{color:red}</style></head>
          <body><nav>menu menu menu</nav>
          <h1>Rolling back</h1><p>Use <code>--force-recreate</code>, not restart.</p>
          <script>tracking()</script>
          <footer>copyright</footer></body></html>`)
	}))
	defer srv.Close()

	c := &Client{Settings: settings{}, HTTP: srv.Client()}
	pages := c.Fetch(context.Background(), []string{srv.URL}, 0)
	if len(pages) != 1 || pages[0].Error != "" {
		t.Fatalf("pages = %+v", pages)
	}
	p := pages[0]
	if p.Title != "Rollback guide" {
		t.Errorf("title = %q", p.Title)
	}
	for _, want := range []string{"Rolling back", "--force-recreate"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("text missing %q: %q", want, p.Text)
		}
	}
	for _, unwanted := range []string{"tracking()", "color:red", "menu menu", "<p>"} {
		if strings.Contains(p.Text, unwanted) {
			t.Errorf("text kept %q: %q", unwanted, p.Text)
		}
	}
}

// One bad URL in a batch must not lose the good ones.
func TestFetchReportsPerURLFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>fine</p></body></html>")
	}))
	defer srv.Close()

	c := &Client{Settings: settings{}, HTTP: srv.Client()}
	pages := c.Fetch(context.Background(), []string{srv.URL + "/ok", srv.URL + "/missing"}, 0)
	if len(pages) != 2 {
		t.Fatalf("got %d pages", len(pages))
	}
	if pages[0].Error != "" || !strings.Contains(pages[0].Text, "fine") {
		t.Errorf("good URL: %+v", pages[0])
	}
	if pages[1].Error == "" || !strings.Contains(pages[1].Error, "404") {
		t.Errorf("bad URL: %+v", pages[1])
	}
	// Order is preserved, so a caller can match answers to what it asked for.
	if !strings.HasSuffix(pages[1].URL, "/missing") {
		t.Errorf("results were reordered: %+v", pages)
	}
}

func TestFetchTruncatesEnormousPages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><p>"+strings.Repeat("word ", 50000)+"</p></body></html>")
	}))
	defer srv.Close()
	c := &Client{Settings: settings{}, HTTP: srv.Client()}
	pages := c.Fetch(context.Background(), []string{srv.URL}, 500)
	if n := len([]rune(pages[0].Text)); n > 600 {
		t.Fatalf("page was not truncated: %d characters", n)
	}
	if !strings.Contains(pages[0].Text, "truncated") {
		t.Error("truncation is silent — a reader cannot tell the page was cut")
	}
}

func TestFetchRefusesNonDocuments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/zip")
		fmt.Fprint(w, "PK\x03\x04binary")
	}))
	defer srv.Close()
	c := &Client{Settings: settings{}, HTTP: srv.Client()}
	pages := c.Fetch(context.Background(), []string{srv.URL}, 0)
	if pages[0].Error == "" {
		t.Fatalf("a zip was read as a document: %+v", pages[0])
	}
}

// rewrite sends absolute provider URLs to the test server.
type rewrite struct{ base string }

func (rw rewrite) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	u.Scheme, u.Host = "http", strings.TrimPrefix(rw.base, "http://")
	clone := r.Clone(r.Context())
	clone.URL = &u
	return http.DefaultTransport.RoundTrip(clone)
}
