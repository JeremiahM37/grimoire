package connectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The four sources added to reach ten. Same discipline as the originals: a
// local server that answers the way the real API does, including the parts each
// one gets awkward about — Discord's newest-first ordering, Notion's block
// tree, Linear's errors-inside-a-200, Readwise's two different cursors.

// fetchAt runs a source against a local server, redirecting its absolute URL.
// The four sources here all hardcode their host, which is correct — an operator
// does not get to point "Discord" at an arbitrary machine — so the transport is
// what gets redirected, exactly as the Slack test already does.
func fetchAt(t *testing.T, kind string, cfg Config, secret, cursor string, srv *httptest.Server) Page {
	t.Helper()
	s, err := Get(kind)
	if err != nil {
		t.Fatal(err)
	}
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	page, err := s.Fetch(context.Background(), Input{
		Config: cfg, Secret: secret, Cursor: cursor, Client: client, Limit: 50,
	})
	if err != nil {
		t.Fatalf("%s fetch: %v", kind, err)
	}
	return page
}

// ---- Discord ---------------------------------------------------------------

// Discord returns messages newest-first, and groups have to come back in
// reading order or a day's transcript is printed backwards.
func TestDiscordGroupsADayIntoOneDocumentInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bot xyz" {
			t.Errorf("Authorization = %q, want the Bot prefix", got)
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "300", "content": "third", "timestamp": "2026-08-01T12:30:00.000000+00:00",
				"author": map[string]any{"username": "carol"}},
			{"id": "200", "content": "second", "timestamp": "2026-08-01T09:15:00.000000+00:00",
				"author": map[string]any{"username": "bob", "bot": true}},
			{"id": "100", "content": "first", "timestamp": "2026-07-31T08:00:00.000000+00:00",
				"author": map[string]any{"username": "alice"}},
		})
	}))
	defer srv.Close()

	page := fetchAt(t, "discord", Config{"channel": "123", "channel_name": "eng"}, "xyz", "", srv)
	if len(page.Docs) != 2 {
		t.Fatalf("got %d documents, want one per day", len(page.Docs))
	}
	if page.Docs[0].Title != "#eng — 2026-07-31" {
		t.Errorf("first document is %q; days must come back oldest-first", page.Docs[0].Title)
	}
	day := page.Docs[1].Body
	if strings.Index(day, "second") > strings.Index(day, "third") {
		t.Error("messages within a day are in reverse order — the API returns " +
			"newest-first and the transcript reads backwards")
	}
	if !strings.Contains(day, "bob (bot)") {
		t.Error("bot authors are not marked, so an agent cannot tell a human from a webhook")
	}
	if page.Cursor != "300" {
		t.Errorf("cursor = %q, want the highest snowflake seen", page.Cursor)
	}
}

// A channel name instead of an id is the commonest setup mistake, and the API
// answers it with an opaque 404.
func TestDiscordRejectsANonNumericChannel(t *testing.T) {
	s, _ := Get("discord")
	_, err := s.Fetch(context.Background(), Input{Config: Config{"channel": "#engineering"}})
	if err == nil {
		t.Fatal("a channel name was accepted as an id")
	}
	if !strings.Contains(err.Error(), "Developer Mode") {
		t.Errorf("error does not say how to get the id: %v", err)
	}
}

func TestDiscordSkipsMessagesWithNoText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"id": "1", "content": "", "timestamp": "2026-08-01T10:00:00+00:00",
				"author": map[string]any{"username": "a"}},
		})
	}))
	defer srv.Close()
	page := fetchAt(t, "discord", Config{"channel": "1"}, "t", "", srv)
	if len(page.Docs) != 0 {
		t.Errorf("an image-only message produced a document: %q", page.Docs[0].Body)
	}
}

// ---- Notion ----------------------------------------------------------------

func TestNotionConvertsBlocksToMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Notion-Version") == "" {
			t.Error("Notion-Version header missing; the API rejects requests without it")
		}
		if strings.Contains(r.URL.Path, "/blocks/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
				{"type": "heading_1", "heading_1": map[string]any{
					"rich_text": []map[string]any{{"plain_text": "Runbook"}}}},
				{"type": "paragraph", "paragraph": map[string]any{
					"rich_text": []map[string]any{
						{"plain_text": "restart ", "annotations": map[string]any{}},
						{"plain_text": "nginx", "annotations": map[string]any{"code": true}},
					}}},
				{"type": "to_do", "to_do": map[string]any{"checked": true,
					"rich_text": []map[string]any{{"plain_text": "page oncall"}}}},
				{"type": "some_future_block", "some_future_block": map[string]any{
					"rich_text": []map[string]any{{"plain_text": "still readable"}}}},
			}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{
				"id": "p1", "object": "page", "url": "https://notion.so/p1",
				"last_edited_time": "2026-08-01T00:00:00.000Z",
				"properties": map[string]any{"Name": map[string]any{
					"type": "title", "title": []map[string]any{{"plain_text": "Deploy runbook"}}}},
			}},
			"has_more": false, "next_cursor": nil,
		})
	}))
	defer srv.Close()

	page := fetchAt(t, "notion", Config{}, "ntn_x", "", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(page.Docs))
	}
	d := page.Docs[0]
	if d.Title != "Deploy runbook" {
		t.Errorf("title = %q — it lives in whichever property has type 'title'", d.Title)
	}
	for _, want := range []string{"# Runbook", "`nginx`", "- [x] page oncall"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("body is missing %q:\n%s", want, d.Body)
		}
	}
	// A block type nobody has written a case for must still contribute its text.
	// Dropping it silently loses a paragraph and nobody finds out.
	if !strings.Contains(d.Body, "still readable") {
		t.Error("an unknown block type vanished instead of degrading to its text")
	}
}

func TestNotionRequiresASecret(t *testing.T) {
	s, _ := Get("notion")
	if _, err := s.Fetch(context.Background(), Input{Config: Config{}}); err == nil {
		t.Fatal("notion fetched with no integration secret")
	}
}

// ---- Linear ----------------------------------------------------------------

// GraphQL reports failures in a 200 body. Unchecked, a revoked key looks like a
// source that simply has no issues, forever.
func TestLinearSurfacesGraphQLErrorsDespiteHTTP200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"errors": []map[string]any{{"message": "Authentication required"}},
		})
	}))
	defer srv.Close()
	s, _ := Get("linear")
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	_, err := s.Fetch(context.Background(), Input{
		Config: Config{}, Secret: "lin_bad", Client: client, Limit: 10})
	if err == nil {
		t.Fatal("a GraphQL error inside a 200 was treated as an empty page")
	}
	if !strings.Contains(err.Error(), "Authentication required") {
		t.Errorf("the API's reason was dropped: %v", err)
	}
}

func TestLinearBuildsIssuesWithComments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body["query"].(string), "issues(") {
			t.Error("the GraphQL query was not sent")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{
			"issues": map[string]any{
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "cur1"},
				"nodes": []map[string]any{{
					"id": "i1", "identifier": "ENG-12", "title": "Fix the watcher",
					"description": "It misses external edits.", "url": "https://linear.app/i1",
					"updatedAt": "2026-08-02T00:00:00Z",
					"state":     map[string]any{"name": "In Progress"},
					"team":      map[string]any{"key": "ENG", "name": "Engineering"},
					"assignee":  map[string]any{"name": "Dana"},
					"labels":    map[string]any{"nodes": []map[string]any{{"name": "bug"}}},
					"comments": map[string]any{"nodes": []map[string]any{
						{"body": "reproduced", "createdAt": "2026-08-02T01:00:00Z",
							"user": map[string]any{"name": "Eve"}}}},
				}},
			}}})
	}))
	defer srv.Close()

	page := fetchAt(t, "linear", Config{"team": "ENG"}, "lin_ok", "", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d documents, want 1", len(page.Docs))
	}
	d := page.Docs[0]
	if d.Title != "ENG-12 Fix the watcher" {
		t.Errorf("title = %q, want the identifier prefixed", d.Title)
	}
	for _, want := range []string{"In Progress", "It misses external edits.", "reproduced", "Eve"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("body missing %q:\n%s", want, d.Body)
		}
	}
	if d.Meta["labels"] != "bug" {
		t.Errorf("labels = %q", d.Meta["labels"])
	}
	if page.Cursor != "cur1" || !page.More {
		t.Errorf("pagination not threaded: cursor=%q more=%v", page.Cursor, page.More)
	}
}

// ---- Readwise --------------------------------------------------------------

func TestReadwiseGroupsHighlightsPerBook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token rw" {
			t.Errorf("Authorization = %q, want the Token prefix", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"nextPageCursor": nil,
			"results": []map[string]any{{
				"user_book_id": 77, "title": "Thinking in Systems", "author": "Donella Meadows",
				"category": "books", "source_url": "https://example.org/b",
				"highlights": []map[string]any{
					{"id": 1, "text": "A system is more than the sum of its parts.",
						"note": "cf. the vault", "updated": "2026-08-03T00:00:00Z",
						"tags": []map[string]any{{"name": "systems thinking"}}},
					{"id": 2, "text": "", "updated": "2026-08-01T00:00:00Z"},
				}}}})
	}))
	defer srv.Close()

	page := fetchAt(t, "readwise", Config{}, "rw", "", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d documents, want one per book", len(page.Docs))
	}
	d := page.Docs[0]
	if !strings.Contains(d.Body, "> A system is more than the sum of its parts.") {
		t.Errorf("highlight is not quoted — it is somebody else's words:\n%s", d.Body)
	}
	if !strings.Contains(d.Body, "cf. the vault") {
		t.Error("the reader's own note was dropped, which is the valuable half")
	}
	if !strings.Contains(d.Body, "#systems-thinking") {
		t.Error("tag was not converted to a usable one (spaces break #tags)")
	}
	if d.Meta["highlights"] != "2" {
		t.Errorf("highlight count = %q, want the source's own total", d.Meta["highlights"])
	}
	// With no further pages the cursor becomes a timestamp, so the next sync is
	// incremental instead of re-pulling the whole library.
	if page.More || page.Cursor != "2026-08-03T00:00:00Z" {
		t.Errorf("cursor = %q more=%v, want the newest update time", page.Cursor, page.More)
	}
}

func TestReadwiseFiltersByCategory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{
			{"user_book_id": 1, "title": "A tweet", "category": "tweets",
				"highlights": []map[string]any{{"id": 1, "text": "hot take"}}},
			{"user_book_id": 2, "title": "A book", "category": "books",
				"highlights": []map[string]any{{"id": 2, "text": "considered"}}},
		}})
	}))
	defer srv.Close()
	page := fetchAt(t, "readwise", Config{"category": "books"}, "rw", "", srv)
	if len(page.Docs) != 1 || page.Docs[0].Title != "A book" {
		t.Fatalf("category filter did not apply: %+v", page.Docs)
	}
}

// ---- the set as a whole ----------------------------------------------------

// Ten sources is the claim the README makes; this is what keeps it true.
func TestTenSourcesAreRegisteredAndDescribed(t *testing.T) {
	want := []string{"confluence", "discord", "gdrive", "github", "jira",
		"linear", "notion", "readwise", "rss", "slack"}
	have := map[string]Kind{}
	for _, k := range Kinds() {
		have[k.Kind] = k
	}
	for _, kind := range want {
		k, ok := have[kind]
		if !ok {
			t.Errorf("%s is not registered", kind)
			continue
		}
		if k.Name == "" || k.Help == "" || k.DefaultPrefix == "" {
			t.Errorf("%s is under-described: %+v", kind, k)
		}
		// A source needing a credential must say where to get one. "Unauthorized"
		// with no further guidance is the most common way an integration dies.
		if kind != "rss" && k.SecretHelp == "" {
			t.Errorf("%s does not say how to obtain its credential", kind)
		}
	}
	if len(have) < len(want) {
		t.Errorf("registered %d kinds, want at least %d", len(have), len(want))
	}
}
