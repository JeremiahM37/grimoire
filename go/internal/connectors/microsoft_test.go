package connectors

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func graphClient(srv *httptest.Server) *http.Client {
	c := srv.Client()
	c.Transport = rewriteHost{srv.URL, c.Transport}
	return c
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func msg(id, conv, subj, from, date string, extra map[string]any) map[string]any {
	m := map[string]any{
		"id": id, "conversationId": conv, "subject": subj, "receivedDateTime": date,
		"from":          map[string]any{"emailAddress": map[string]string{"name": "X", "address": from}},
		"toRecipients":  []any{map[string]any{"emailAddress": map[string]string{"address": "me@corp.com"}}},
		"body":          map[string]string{"contentType": "text", "content": "body of " + id},
		"webLink":       "https://outlook.example/" + id,
		"isDraft":       false,
		"hasAttachment": false,
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func outlookServer(t *testing.T, filters *[]string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth = %q", r.Header.Get("Authorization"))
		}
		q := r.URL.Query()
		switch {
		case r.URL.Path == "/v1.0/me":
			writeJSON(w, map[string]string{"mail": "Me@Corp.com", "userPrincipalName": "me@corp.com"})
		case strings.HasPrefix(r.URL.Path, "/v1.0/me/mailFolders/"):
			*filters = append(*filters, q.Get("$filter"))
			if strings.Contains(q.Get("$filter"), "2026-09-02") {
				writeJSON(w, map[string]any{"value": []any{}})
				return
			}
			if !strings.Contains(r.URL.Path, "inbox") {
				writeJSON(w, map[string]any{"value": []any{}})
				return
			}
			writeJSON(w, map[string]any{"value": []any{
				msg("m1", "c1", "Plan", "bob@x.com", "2026-09-01T10:00:00Z", nil),
				msg("m3", "c2", "Draft thing", "me@corp.com", "2026-09-01T11:00:00Z", map[string]any{"isDraft": true}),
			}})
		case r.URL.Path == "/v1.0/me/messages" && q.Get("$search") != "":
			if q.Get("$search") != `"say \"hi\""` {
				t.Errorf("$search = %q", q.Get("$search"))
			}
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "m9", "conversationId": "c9", "subject": "Hello", "bodyPreview": "snip", "webLink": "u"},
			}})
		case r.URL.Path == "/v1.0/me/messages" && strings.Contains(q.Get("$filter"), "conversationId eq 'c1'"):
			writeJSON(w, map[string]any{"value": []any{
				msg("m2", "c1", "Re: Plan", "me@corp.com", "2026-09-01T12:00:00Z", nil),
				msg("m1", "c1", "Plan", "bob@x.com", "2026-09-01T10:00:00Z", map[string]any{
					"attachments": []any{map[string]any{"name": "a.pdf", "contentType": "application/pdf", "size": 12}},
				}),
				msg("d1", "c1", "Re: Plan", "me@corp.com", "2026-09-01T13:00:00Z", map[string]any{"isDraft": true}),
			}})
		case r.URL.Path == "/v1.0/me/messages" && strings.Contains(q.Get("$filter"), "conversationId eq 'own'"):
			writeJSON(w, map[string]any{"value": []any{
				msg("o1", "own", "Mine", "ME@corp.com", "2026-09-01T09:00:00Z", nil),
			}})
		case r.URL.Path == "/v1.0/me/messages" && r.Method == http.MethodPost:
			var got map[string]any
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &got)
			if got["subject"] != "Hi" || len(got["toRecipients"].([]any)) != 2 || got["ccRecipients"] == nil {
				t.Errorf("draft payload = %s", b)
			}
			writeJSON(w, map[string]string{"id": "dr1", "webLink": "https://outlook.example/dr1"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
}

func outlookInput(srv *httptest.Server, cursor string) Input {
	return Input{Config: Config{"folders": "inbox,sentitems", "since": "2026-08-01"}, Secret: "tok",
		Cursor: cursor, Client: graphClient(srv), Limit: 50}
}

func TestOutlookSyncGroupsConversationAndSkipsDrafts(t *testing.T) {
	var filters []string
	srv := outlookServer(t, &filters)
	defer srv.Close()
	page, err := outlook{}.Fetch(context.Background(), outlookInput(srv, ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Docs) != 1 {
		t.Fatalf("docs = %d, want 1 (draft-only conversation skipped)", len(page.Docs))
	}
	d := page.Docs[0]
	if d.ExternalID != "outlook:c1" || d.Meta["source"] != "outlook" || d.Meta["message_count"] != "2" {
		t.Errorf("doc = %+v", d)
	}
	if d.URL != "https://outlook.example/m2" {
		t.Errorf("URL = %q, want latest non-draft message", d.URL)
	}
	if !strings.Contains(d.Body, "a.pdf (application/pdf, 12 bytes)") || strings.Index(d.Body, "body of m1") > strings.Index(d.Body, "body of m2") {
		t.Errorf("body = %s", d.Body)
	}
	if d.Own {
		t.Error("thread with a stranger's message must not be Own")
	}
	if page.Cursor != "2026-09-01T10:00:00Z" {
		t.Errorf("cursor = %q", page.Cursor)
	}
	if !strings.Contains(filters[0], "2026-08-01") {
		t.Errorf("first filter = %q", filters[0])
	}
}

func TestOutlookIncrementalUsesCursor(t *testing.T) {
	var filters []string
	srv := outlookServer(t, &filters)
	defer srv.Close()
	page, err := outlook{}.Fetch(context.Background(), outlookInput(srv, "2026-09-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Docs) != 0 || page.Cursor != "2026-09-02T00:00:00Z" {
		t.Errorf("page = %+v", page)
	}
	if len(filters) == 0 || !strings.Contains(filters[0], "receivedDateTime gt 2026-09-02T00:00:00Z") {
		t.Errorf("filters = %v", filters)
	}
}

func TestOutlookOwnOnlyWhenAllFromMailbox(t *testing.T) {
	var filters []string
	srv := outlookServer(t, &filters)
	defer srv.Close()
	msgs, err := outlook{}.conversation(context.Background(), outlookInput(srv, ""), "own")
	if err != nil {
		t.Fatal(err)
	}
	own, _ := mailbox(context.Background(), outlookInput(srv, ""))
	if !(outlook{}).document(msgs, own).Own {
		t.Error("all messages from the mailbox should be Own")
	}
	item, err := outlook{}.Read(context.Background(), outlookInput(srv, ""), "outlook:own")
	if err != nil || item.Title != "Mine" {
		t.Errorf("item = %+v err=%v", item, err)
	}
}

func TestOutlookSearchAndRead(t *testing.T) {
	var filters []string
	srv := outlookServer(t, &filters)
	defer srv.Close()
	in := outlookInput(srv, "")
	hits, err := outlook{}.Search(context.Background(), in, `say "hi"`, 5)
	if err != nil || len(hits) != 1 || hits[0].ID != "c9" || hits[0].Snippet != "snip" {
		t.Errorf("hits = %+v err=%v", hits, err)
	}
	if _, err := (outlook{}).Search(context.Background(), in, "a\nb", 5); err == nil {
		t.Error("newline query accepted")
	}
	item, err := outlook{}.Read(context.Background(), in, "c1")
	if err != nil || len(item.Attachments) != 1 || !strings.Contains(item.Body, "body of m2") {
		t.Errorf("item = %+v err=%v", item, err)
	}
}

func TestOutlookCreateDraft(t *testing.T) {
	var filters []string
	srv := outlookServer(t, &filters)
	defer srv.Close()
	in := outlookInput(srv, "")
	res, err := outlook{}.Act(context.Background(), in, "create_draft", map[string]string{
		"to": "a@x.com, b@x.com", "cc": "c@x.com", "subject": "Hi", "body": "hello"})
	if err != nil || res.ID != "dr1" || res.URL != "https://outlook.example/dr1" {
		t.Errorf("res = %+v err=%v", res, err)
	}
	for _, p := range []map[string]string{
		{"to": "a@x.com\nBcc: z@x.com", "subject": "Hi", "body": "b"},
		{"to": "a@x.com", "subject": "Hi\r\nBcc: z", "body": "b"},
		{"to": "a@x.com", "cc": "c@x.com\n", "subject": "Hi", "body": "b"},
	} {
		if _, err := (outlook{}).Act(context.Background(), in, "create_draft", p); err == nil {
			t.Errorf("header injection accepted: %v", p)
		}
	}
	if _, err := (outlook{}).Act(context.Background(), in, "send", nil); err == nil {
		t.Error("unknown action accepted")
	}
	if err := Validate("outlook", Config{"actions": "send"}); err == nil {
		t.Error("send should not validate as an action")
	}
}

// ---- OneDrive ---------------------------------------------------------------

func onedriveServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := func(id string) map[string]any {
			return map[string]any{"user": map[string]string{"id": id, "displayName": id}}
		}
		switch {
		case r.URL.Path == "/v1.0/me":
			writeJSON(w, map[string]string{"id": "u1"})
		case r.URL.Path == "/v1.0/me/drive/root/delta" && r.URL.Query().Get("token") == "":
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "f1", "name": "notes.md", "size": 5, "webUrl": "w1", "lastModifiedDateTime": "2026-09-01T00:00:00Z",
					"file": map[string]string{"mimeType": "text/markdown"}, "createdBy": user("u1"), "lastModifiedBy": user("u1")},
				map[string]any{"id": "f2", "name": "report.docx", "size": 999, "webUrl": "w2",
					"file":      map[string]string{"mimeType": "application/vnd.openxmlformats-officedocument.wordprocessingml.document"},
					"createdBy": user("u1"), "lastModifiedBy": user("u2")},
				map[string]any{"id": "d1", "name": "Dir", "folder": map[string]any{}},
				map[string]any{"id": "x1", "name": "gone.txt", "deleted": map[string]any{}, "file": map[string]string{"mimeType": "text/plain"}},
			}, "@odata.nextLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=p2"})
		case r.URL.Path == "/v1.0/me/drive/root/delta" && r.URL.Query().Get("token") == "p2":
			writeJSON(w, map[string]any{"value": []any{}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=final"})
		case r.URL.Path == "/v1.0/me/drive/root/delta" && r.URL.Query().Get("token") == "final":
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "f3", "name": "data.csv", "file": map[string]string{"mimeType": "application/octet-stream"}},
			}, "@odata.deltaLink": "https://graph.microsoft.com/v1.0/me/drive/root/delta?token=final2"})
		case r.URL.Path == "/v1.0/me/drive/items/f1/content":
			io.WriteString(w, "hello notes")
		case r.URL.Path == "/v1.0/me/drive/items/f3/content":
			io.WriteString(w, "a,b\n1,2")
		case r.URL.Path == "/v1.0/me/drive/items/f2/content":
			t.Error("docx content must not be downloaded")
		case r.URL.Path == "/v1.0/me/drive/items/f2":
			writeJSON(w, map[string]any{"id": "f2", "name": "report.docx", "size": 999, "webUrl": "w2",
				"file": map[string]string{"mimeType": "application/msword"}})
		case r.URL.Path == "/v1.0/me/drive/items/f1":
			writeJSON(w, map[string]any{"id": "f1", "name": "notes.md", "webUrl": "w1", "file": map[string]string{"mimeType": "text/markdown"}})
		case r.URL.Path == "/v1.0/me/drive/root/search(q='it''s')":
			writeJSON(w, map[string]any{"value": []any{
				map[string]any{"id": "f1", "name": "notes.md", "webUrl": "w1", "file": map[string]string{"mimeType": "text/markdown"}},
				map[string]any{"id": "d1", "name": "Dir", "folder": map[string]any{}},
			}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.String())
			http.NotFound(w, r)
		}
	}))
}

func TestOneDriveDeltaSyncAndIncremental(t *testing.T) {
	srv := onedriveServer(t)
	defer srv.Close()
	in := Input{Secret: "tok", Client: graphClient(srv), Limit: 50}
	page, err := onedrive{}.Fetch(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Docs) != 2 || page.More || !strings.HasSuffix(page.Cursor, "token=final") || page.Complete {
		t.Fatalf("page = %+v", page)
	}
	md, docx := page.Docs[0], page.Docs[1]
	if md.ExternalID != "onedrive:f1" || md.Body != "hello notes" || !md.Own || md.Meta["mime"] != "text/markdown" {
		t.Errorf("md = %+v", md)
	}
	if !strings.Contains(docx.Body, "Content not extracted") || !strings.Contains(docx.Body, "999") || docx.Own {
		t.Errorf("docx = %+v", docx)
	}
	in.Cursor = page.Cursor
	page, err = onedrive{}.Fetch(context.Background(), in)
	if err != nil || len(page.Docs) != 1 || page.Docs[0].Body != "a,b\n1,2" || !strings.HasSuffix(page.Cursor, "final2") {
		t.Errorf("incremental page = %+v err=%v", page, err)
	}
}

func TestOneDriveLimitReturnsMoreWithNextLink(t *testing.T) {
	srv := onedriveServer(t)
	defer srv.Close()
	page, err := onedrive{}.Fetch(context.Background(), Input{Secret: "tok", Client: graphClient(srv), Limit: 1})
	if err != nil || !page.More || !strings.HasSuffix(page.Cursor, "token=p2") {
		t.Errorf("page = %+v err=%v", page, err)
	}
}

func TestOneDriveRejectsForeignDeltaLink(t *testing.T) {
	srv := onedriveServer(t)
	defer srv.Close()
	_, err := onedrive{}.Fetch(context.Background(), Input{Secret: "tok", Client: graphClient(srv),
		Cursor: "https://evil.example/v1.0/me/drive/root/delta"})
	if err == nil || !strings.Contains(err.Error(), "graph.microsoft.com") {
		t.Errorf("err = %v", err)
	}
}

func TestOneDriveSearchAndRead(t *testing.T) {
	srv := onedriveServer(t)
	defer srv.Close()
	in := Input{Secret: "tok", Client: graphClient(srv)}
	hits, err := onedrive{}.Search(context.Background(), in, "it's", 5)
	if err != nil || len(hits) != 1 || hits[0].ID != "f1" {
		t.Errorf("hits = %+v err=%v", hits, err)
	}
	item, err := onedrive{}.Read(context.Background(), in, "onedrive:f1")
	if err != nil || item.Body != "hello notes" {
		t.Errorf("item = %+v err=%v", item, err)
	}
	item, err = onedrive{}.Read(context.Background(), in, "f2")
	if err != nil || !strings.Contains(item.Body, "Content not extracted") {
		t.Errorf("item = %+v err=%v", item, err)
	}
}

func TestOutlookOneDriveDescribeMentionsConnect(t *testing.T) {
	for _, k := range []string{"outlook", "onedrive"} {
		s, _ := Get(k)
		d := s.Describe()
		if !strings.Contains(d.SecretHelp, "grimoire connect microsoft") {
			t.Errorf("%s SecretHelp missing connect hint", k)
		}
	}
}
