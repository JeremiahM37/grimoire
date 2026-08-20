package connectors

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Every source is tested against a local server that answers like the real one.
// No network, no credentials, and the awkward parts each API actually has —
// Slack's 200-with-ok:false, Jira's ADF descriptions, Confluence's storage
// format, Drive's separate export call — are what the fixtures reproduce.

func fetch(t *testing.T, kind string, cfg Config, secret, cursor string, srv *httptest.Server) Page {
	t.Helper()
	s, err := Get(kind)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Fetch(context.Background(), Input{
		Config: cfg, Secret: secret, Cursor: cursor, Client: srv.Client(), Limit: 50,
	})
	if err != nil {
		t.Fatalf("%s fetch: %v", kind, err)
	}
	return page
}

func TestEveryKindDescribesItself(t *testing.T) {
	kinds := Kinds()
	if len(kinds) < 5 {
		t.Fatalf("only %d connector kinds registered", len(kinds))
	}
	for _, k := range kinds {
		if k.Name == "" || k.Help == "" || k.DefaultPrefix == "" {
			t.Errorf("%s is missing its description: %+v", k.Kind, k)
		}
		for _, f := range k.Fields {
			if f.Label == "" {
				t.Errorf("%s field %q has no label", k.Kind, f.Name)
			}
		}
		// A kind must round-trip through Get, since that is how the runner
		// resolves what an operator configured.
		if _, err := Get(k.Kind); err != nil {
			t.Errorf("registered kind %q does not resolve: %v", k.Kind, err)
		}
	}
	if _, err := Get("nope"); err == nil {
		t.Error("an unknown kind resolved")
	}
}

func TestValidateRejectsMissingRequiredFields(t *testing.T) {
	if err := Validate("jira", Config{"site": "https://x.atlassian.net"}); err == nil {
		t.Error("a jira connector without an email was accepted")
	}
	if err := Validate("jira", Config{
		"site": "https://x.atlassian.net", "email": "a@b.c"}); err != nil {
		t.Errorf("a complete jira config was rejected: %v", err)
	}
}

// Slack answers 200 with ok:false on failure, so a status-code check alone
// reads every error as success and syncs nothing forever.
func TestSlackReportsOkFalseAsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_in_channel"})
	}))
	defer srv.Close()
	s, _ := Get("slack")
	// Slack's endpoint is absolute, so the transport is redirected rather than
	// the URL. Without this the test would call the real api.slack.com — which
	// it did, once, and answered invalid_auth: a test that needs the internet
	// is not a test.
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	_, err := s.Fetch(context.Background(), Input{
		Config: Config{"channels": "C1"}, Secret: "xoxb-test",
		Client: client, Limit: 10,
	})
	if err == nil {
		t.Fatal("slack ok:false was treated as success")
	}
	if !strings.Contains(err.Error(), "not_in_channel") ||
		!strings.Contains(err.Error(), "invite it") {
		t.Fatalf("error does not say how to fix it: %v", err)
	}
}

func TestJiraBuildsIssuesFromADF(t *testing.T) {
	var gotJQL string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotJQL = r.URL.Query().Get("jql")
		fmt.Fprint(w, `{"issues":[{"key":"ENG-7","fields":{
          "summary":"Login times out","updated":"2026-08-19T10:00:00.000+0000",
          "status":{"name":"In Progress"},"project":{"key":"ENG"},
          "reporter":{"displayName":"Alice"},"assignee":{"displayName":"Bob"},
          "labels":["auth"],
          "description":{"type":"doc","content":[
            {"type":"paragraph","content":[{"type":"text","text":"Sessions expire "},
             {"type":"text","text":"early","marks":[{"type":"strong"}]}]}]},
          "comment":{"comments":[{"author":{"displayName":"Bob"},"created":"2026-08-19T11:00:00.000+0000",
            "body":{"type":"doc","content":[{"type":"paragraph","content":[
              {"type":"text","text":"It is the proxy timeout."}]}]}}]}}}]}`)
	}))
	defer srv.Close()

	page := fetch(t, "jira", Config{"site": srv.URL, "email": "a@b.c", "projects": "ENG"},
		"token", "2026-08-01T00:00:00Z", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d docs", len(page.Docs))
	}
	d := page.Docs[0]
	if d.ExternalID != "ENG-7" || !strings.Contains(d.Title, "Login times out") {
		t.Fatalf("doc = %+v", d)
	}
	for _, want := range []string{"Sessions expire **early**", "It is the proxy timeout.",
		"In Progress", "## Comments"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("body missing %q:\n%s", want, d.Body)
		}
	}
	if !strings.Contains(gotJQL, "project in (ENG)") || !strings.Contains(gotJQL, "updated >=") {
		t.Errorf("JQL did not filter by project and cursor: %s", gotJQL)
	}
	if page.Cursor != "2026-08-19T10:00:00Z" {
		t.Errorf("cursor = %q, want the newest issue's update time in RFC3339", page.Cursor)
	}
}

func TestConfluenceConvertsStorageFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Query().Get("cql"), `space in ("ENG")`) {
			t.Errorf("cql did not restrict to the space: %s", r.URL.Query().Get("cql"))
		}
		fmt.Fprint(w, `{"results":[{"id":"12345","title":"Runbook",
          "space":{"key":"ENG"},"version":{"when":"2026-08-19T09:00:00.000Z",
          "by":{"displayName":"Alice"}},
          "body":{"storage":{"value":"<h2>Rollback</h2><p>Use <code>--force</code>.</p><ul><li>step one</li><li>step two</li></ul>"}},
          "_links":{"webui":"/spaces/ENG/pages/12345"}}],"size":1,"limit":50}`)
	}))
	defer srv.Close()

	page := fetch(t, "confluence", Config{"site": srv.URL, "email": "a@b.c", "spaces": "ENG"},
		"token", "", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d docs", len(page.Docs))
	}
	d := page.Docs[0]
	for _, want := range []string{"## Rollback", "--force", "- step one", "- step two"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("converted body missing %q:\n%s", want, d.Body)
		}
	}
	if strings.Contains(d.Body, "<") {
		t.Errorf("markup survived conversion:\n%s", d.Body)
	}
	if !strings.HasSuffix(d.URL, "/wiki/spaces/ENG/pages/12345") {
		t.Errorf("url = %q", d.URL)
	}
}

func TestDriveExportsDocumentsAsText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/export"):
			if r.URL.Query().Get("mimeType") != "text/plain" {
				t.Errorf("export asked for %q", r.URL.Query().Get("mimeType"))
			}
			fmt.Fprint(w, "The quarterly plan, in plain text.")
		default:
			if q := r.URL.Query().Get("q"); !strings.Contains(q, "in parents") {
				t.Errorf("query did not restrict to the folder: %s", q)
			}
			fmt.Fprint(w, `{"files":[{"id":"f1","name":"Quarterly plan",
              "mimeType":"application/vnd.google-apps.document",
              "modifiedTime":"2026-08-19T08:00:00Z",
              "webViewLink":"https://docs.google.com/d/f1",
              "owners":[{"displayName":"Alice"}]}]}`)
		}
	}))
	defer srv.Close()

	// Point the source at the test server by overriding the endpoint host is
	// not possible here (URLs are absolute), so this exercises the shape via
	// the same code path with a rewriting transport.
	client := srv.Client()
	client.Transport = rewriteHost{srv.URL, client.Transport}
	s, _ := Get("gdrive")
	page, err := s.Fetch(context.Background(), Input{
		Config: Config{"folder": "F1"}, Secret: "token", Client: client, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Docs) != 1 || page.Docs[0].Body != "The quarterly plan, in plain text." {
		t.Fatalf("docs = %+v", page.Docs)
	}
	if page.Cursor != "2026-08-19T08:00:00Z" {
		t.Errorf("cursor = %q", page.Cursor)
	}
}

func TestGitHubBuildsIssuesWithComments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[{"body":"Fixed in #12","created_at":"2026-08-19T10:00:00Z",
              "user":{"login":"bob"}}]`)
		default:
			fmt.Fprint(w, `[{"number":11,"title":"Deploy fails","body":"It 502s",
              "state":"open","html_url":"https://github.com/o/r/issues/11",
              "updated_at":"2026-08-19T10:05:00Z","comments":1,
              "user":{"login":"alice"},"labels":[{"name":"bug"}]}]`)
		}
	}))
	defer srv.Close()

	page := fetch(t, "github", Config{"repo": "o/r", "api": srv.URL}, "ghp_x", "", srv)
	if len(page.Docs) != 1 {
		t.Fatalf("got %d docs", len(page.Docs))
	}
	d := page.Docs[0]
	if d.ExternalID != "o/r#11" {
		t.Errorf("external id = %q", d.ExternalID)
	}
	for _, want := range []string{"It 502s", "Fixed in #12", "opened by alice"} {
		if !strings.Contains(d.Body, want) {
			t.Errorf("body missing %q:\n%s", want, d.Body)
		}
	}
	if d.Meta["labels"] != "bug" {
		t.Errorf("labels = %q", d.Meta["labels"])
	}
}

func TestFeedReadsRSSAndAtom(t *testing.T) {
	rss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><rss><channel><title>Changelog</title>
          <item><title>v2 released</title><link>https://x/1</link><guid>1</guid>
          <pubDate>Tue, 19 Aug 2026 10:00:00 +0000</pubDate>
          <description>&lt;p&gt;Now with &lt;b&gt;connectors&lt;/b&gt;.&lt;/p&gt;</description>
          </item></channel></rss>`)
	}))
	defer rss.Close()
	page := fetch(t, "rss", Config{"url": rss.URL}, "", "", rss)
	if len(page.Docs) != 1 {
		t.Fatalf("rss docs = %d", len(page.Docs))
	}
	if got := page.Docs[0].Body; !strings.Contains(got, "Now with **connectors**") &&
		!strings.Contains(got, "Now with connectors") {
		t.Errorf("rss body = %q", got)
	}
	if page.Docs[0].Updated != "2026-08-19T10:00:00Z" {
		t.Errorf("rss date = %q", page.Docs[0].Updated)
	}

	atom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<?xml version="1.0"?><feed><title>Blog</title>
          <entry><title>On retrieval</title><id>urn:1</id>
          <updated>2026-08-18T12:00:00Z</updated>
          <link href="https://x/post" rel="alternate"/>
          <content>&lt;p&gt;Hybrid wins when the corpus does not fit.&lt;/p&gt;</content>
          <author><name>Alice</name></author></entry></feed>`)
	}))
	defer atom.Close()
	page = fetch(t, "rss", Config{"url": atom.URL}, "", "", atom)
	if len(page.Docs) != 1 || page.Docs[0].URL != "https://x/post" {
		t.Fatalf("atom docs = %+v", page.Docs)
	}
	if page.Docs[0].Author != "Alice" {
		t.Errorf("atom author = %q", page.Docs[0].Author)
	}
}

// rewriteHost sends absolute requests to the test server instead, so a source
// with hardcoded endpoints (Slack, Drive) can be exercised without network.
type rewriteHost struct {
	base string
	next http.RoundTripper
}

func (rw rewriteHost) RoundTrip(r *http.Request) (*http.Response, error) {
	u := *r.URL
	base := strings.TrimPrefix(rw.base, "http://")
	u.Scheme, u.Host = "http", base
	clone := r.Clone(r.Context())
	clone.URL = &u
	next := rw.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(clone)
}

// ------------------------------------------------------------------ runner

// fakeWriter records what the runner wrote, standing in for the vault.
type fakeWriter struct {
	notes   map[string]string
	fm      map[string]map[string]any
	deleted []string
	writes  int
}

func newFakeWriter() *fakeWriter {
	return &fakeWriter{notes: map[string]string{}, fm: map[string]map[string]any{}}
}

func (f *fakeWriter) WriteNote(path, body string, frontmatter map[string]any) (string, error) {
	f.notes[path] = body
	f.fm[path] = frontmatter
	f.writes++
	return path, nil
}

func (f *fakeWriter) DeleteNote(path string) error {
	delete(f.notes, path)
	f.deleted = append(f.deleted, path)
	return nil
}

type fakeSecrets map[string]string

func (f fakeSecrets) Get(name string) (string, error) {
	v, ok := f[name]
	if !ok {
		return "", fmt.Errorf("vault is locked")
	}
	return v, nil
}

// stubSource returns whatever a test hands it.
type stubSource struct {
	pages []Page
	calls int
	fail  error
}

func (s *stubSource) Kind() string { return "stub" }
func (s *stubSource) Describe() Kind {
	return Kind{Kind: "stub", Name: "Stub", Help: "test", DefaultPrefix: "stub"}
}
func (s *stubSource) Fetch(ctx context.Context, in Input) (Page, error) {
	if s.fail != nil {
		return Page{}, s.fail
	}
	if s.calls >= len(s.pages) {
		return Page{}, nil
	}
	p := s.pages[s.calls]
	s.calls++
	return p, nil
}

func runnerFixture(t *testing.T, src *stubSource) (*Runner, *fakeWriter, *Store, Connector) {
	t.Helper()
	Register(src)
	database := testDB(t)
	store := NewStore(database)
	w := newFakeWriter()
	c := Connector{ID: "c1", Kind: "stub", Name: "Test", Prefix: "connectors/stub",
		Secret: "tok", Enabled: true, Config: Config{}, Created: "2026-08-19T00:00:00Z"}
	if err := store.Save(c); err != nil {
		t.Fatal(err)
	}
	return &Runner{Store: store, Writer: w, Secrets: fakeSecrets{"tok": "s3cret"},
		Limit: 10, MaxPages: 3}, w, store, c
}

// A second sync of an unchanged document must not rewrite the note: rewriting
// means re-embedding, which is the expensive half of ingestion.
func TestRunnerSkipsUnchangedDocuments(t *testing.T) {
	doc := Document{ExternalID: "1", Title: "Runbook", Body: "rollback steps",
		Updated: "2026-08-19T10:00:00Z"}
	src := &stubSource{pages: []Page{
		{Docs: []Document{doc}, Cursor: "2026-08-19T10:00:00Z"},
		{Docs: []Document{doc}, Cursor: "2026-08-19T10:00:00Z"},
	}}
	r, w, store, _ := runnerFixture(t, src)

	first, err := r.Run(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Written != 1 || w.writes != 1 {
		t.Fatalf("first run wrote %d notes (%+v)", w.writes, first)
	}
	second, err := r.Run(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Written != 0 || second.Skipped != 1 || w.writes != 1 {
		t.Fatalf("unchanged document was rewritten: %+v (writes=%d)", second, w.writes)
	}
	saved, _ := store.Get("c1")
	if saved.Cursor != "2026-08-19T10:00:00Z" || !saved.LastOK {
		t.Fatalf("connector state = %+v", saved)
	}
}

// An edited document updates the SAME note rather than creating a second one.
func TestRunnerUpdatesInPlace(t *testing.T) {
	src := &stubSource{pages: []Page{
		{Docs: []Document{{ExternalID: "1", Title: "Runbook", Body: "v1"}}},
		{Docs: []Document{{ExternalID: "1", Title: "Runbook rewritten", Body: "v2"}}},
	}}
	r, w, _, _ := runnerFixture(t, src)
	if _, err := r.Run(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Run(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if len(w.notes) != 1 {
		t.Fatalf("an edit created a second note: %v", keysOf(w.notes))
	}
	for _, body := range w.notes {
		if !strings.Contains(body, "v2") {
			t.Fatalf("note was not updated: %q", body)
		}
	}
}

// A failure must be recorded where an operator can see it, with the cursor
// left where the last successful page ended.
func TestRunnerRecordsFailures(t *testing.T) {
	src := &stubSource{fail: fmt.Errorf("401 — the credential was rejected")}
	r, _, store, _ := runnerFixture(t, src)
	if _, err := r.Run(context.Background(), "c1"); err == nil {
		t.Fatal("a failing fetch reported success")
	}
	saved, _ := store.Get("c1")
	if saved.LastOK || !strings.Contains(saved.LastErr, "credential was rejected") {
		t.Fatalf("failure not recorded: %+v", saved)
	}
	if saved.LastRun == "" {
		t.Error("a failed run left no timestamp, so nothing shows it was tried")
	}
}

// A locked vault is the most common reason a connector stops working, and the
// least obvious from a generic error.
func TestRunnerExplainsALockedVault(t *testing.T) {
	src := &stubSource{}
	r, _, store, _ := runnerFixture(t, src)
	r.Secrets = fakeSecrets{} // nothing resolves
	if _, err := r.Run(context.Background(), "c1"); err == nil {
		t.Fatal("a missing credential reported success")
	}
	saved, _ := store.Get("c1")
	if !strings.Contains(saved.LastErr, "vault") {
		t.Fatalf("error does not mention the vault: %q", saved.LastErr)
	}
}

func TestRunnerFollowsPagesToTheLimit(t *testing.T) {
	page := func(id string) Page {
		return Page{Docs: []Document{{ExternalID: id, Title: "Doc " + id, Body: id}},
			Cursor: id, More: true}
	}
	src := &stubSource{pages: []Page{page("1"), page("2"), page("3"), page("4")}}
	r, w, _, _ := runnerFixture(t, src)
	res, err := r.Run(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Written != 3 || w.writes != 3 {
		t.Fatalf("MaxPages was not honoured: wrote %d", w.writes)
	}
	if res.Cursor != "3" {
		t.Fatalf("cursor = %q, want the last page fetched", res.Cursor)
	}
}

func TestNotePathsAreStableAndSafe(t *testing.T) {
	for _, c := range []struct{ title, id, want string }{
		{"Runbook", "ENG-7", "connectors/x/runbook-eng-7.md"},
		{"../../etc/passwd", "1", "connectors/x/etc-passwd-1.md"},
		{"", "abc", "connectors/x/abc.md"},
		{"Ünïcödé title", "9", "connectors/x/ünïcödé-title-9.md"},
	} {
		got := notePath("connectors/x", Document{Title: c.title, ExternalID: c.id})
		if got != c.want {
			t.Errorf("notePath(%q,%q) = %q, want %q", c.title, c.id, got, c.want)
		}
	}
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Deletion is the one thing an incremental sync cannot see: a document that was
// deleted did not change. Only a source that just listed everything can say so,
// and saying it wrongly deletes a vault — so the claim is explicit and tested
// from both sides.
func TestCompletePagesRemoveDocumentsTheSourceNoLongerHas(t *testing.T) {
	src := &stubSource{pages: []Page{
		{Docs: []Document{
			{ExternalID: "1", Title: "Kept", Body: "one"},
			{ExternalID: "2", Title: "Doomed", Body: "two"},
		}, Complete: true},
		// Second sync: the source no longer lists "2".
		{Docs: []Document{{ExternalID: "1", Title: "Kept", Body: "one"}}, Complete: true},
	}}
	r, w, _, _ := runnerFixture(t, src)
	if _, err := r.Run(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if len(w.notes) != 2 {
		t.Fatalf("first sync wrote %d notes", len(w.notes))
	}
	res, err := r.Run(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed %d, want the one the source dropped", res.Removed)
	}
	if len(w.notes) != 1 {
		t.Fatalf("notes after reap = %v", keysOf(w.notes))
	}
	for path := range w.notes {
		if strings.Contains(path, "doomed") {
			t.Fatalf("the deleted document's note survived: %s", path)
		}
	}
}

// The dangerous direction: an INCREMENTAL page says nothing about what still
// exists, so absence from it must never delete anything.
func TestIncrementalPagesNeverDeleteAnything(t *testing.T) {
	src := &stubSource{pages: []Page{
		{Docs: []Document{
			{ExternalID: "1", Title: "One", Body: "one"},
			{ExternalID: "2", Title: "Two", Body: "two"},
		}},
		// A normal incremental page: only what changed. Both notes must stay.
		{Docs: []Document{{ExternalID: "1", Title: "One", Body: "one edited"}}},
	}}
	r, w, _, _ := runnerFixture(t, src)
	if _, err := r.Run(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	res, err := r.Run(context.Background(), "c1")
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Fatalf("an incremental page removed %d notes", res.Removed)
	}
	if len(w.notes) != 2 || len(w.deleted) != 0 {
		t.Fatalf("notes=%v deleted=%v", keysOf(w.notes), w.deleted)
	}
}

// Routing is how a source's structure reaches Grimoire's access boundaries: a
// Confluence space or a Slack channel lands in its own folder, and a folder is
// what a Grimoire space is drawn around. It is not a copy of the source's ACL —
// that limit is documented — but it is what makes "HR's wiki is not
// Engineering's wiki" expressible at all.
func TestDocumentsRouteToFoldersBySourceStructure(t *testing.T) {
	docs := []Document{
		{ExternalID: "1", Title: "Runbook", Body: "x", Meta: map[string]string{"space": "ENG"}},
		{ExternalID: "2", Title: "Handbook", Body: "y", Meta: map[string]string{"space": "HR"}},
		{ExternalID: "3", Title: "Stray", Body: "z"}, // no value for the field
	}

	t.Run("explicit map", func(t *testing.T) {
		src := &stubSource{pages: []Page{{Docs: docs}}}
		r, w, store, c := runnerFixture(t, src)
		c.Config = Config{"route_by": "space", "route_map": "ENG=team/eng, HR=hr/wiki"}
		if err := store.Save(c); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		paths := keysOf(w.notes)
		want := map[string]bool{
			"team/eng/runbook-1.md":      true,
			"hr/wiki/handbook-2.md":      true,
			"connectors/stub/stray-3.md": true, // unmapped falls back, never dropped
		}
		for _, p := range paths {
			if !want[p] {
				t.Errorf("unexpected destination %q", p)
			}
			delete(want, p)
		}
		for p := range want {
			t.Errorf("missing destination %q (got %v)", p, paths)
		}
	})

	t.Run("no map: a subfolder per value", func(t *testing.T) {
		src := &stubSource{pages: []Page{{Docs: docs}}}
		r, w, store, c := runnerFixture(t, src)
		c.Config = Config{"route_by": "space"}
		if err := store.Save(c); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Run(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, p := range keysOf(w.notes) {
			found[p] = true
		}
		for _, want := range []string{
			"connectors/stub/eng/runbook-1.md",
			"connectors/stub/hr/handbook-2.md",
			"connectors/stub/stray-3.md",
		} {
			if !found[want] {
				t.Errorf("missing %q (got %v)", want, keysOf(w.notes))
			}
		}
	})

	t.Run("no routing configured", func(t *testing.T) {
		src := &stubSource{pages: []Page{{Docs: docs}}}
		r, w, _, _ := runnerFixture(t, src)
		if _, err := r.Run(context.Background(), "c1"); err != nil {
			t.Fatal(err)
		}
		for _, p := range keysOf(w.notes) {
			if !strings.HasPrefix(p, "connectors/stub/") || strings.Count(p, "/") != 2 {
				t.Errorf("routing happened without being asked for: %q", p)
			}
		}
	})
}
