package connectors

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/mail"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Gmail, through the Gmail API (OAuth), as opposed to the generic IMAP source
// which needs an app password. One document per thread.

func init() { Register(gmail{}) }

type gmail struct{}

const gmailBase = "https://gmail.googleapis.com/gmail/v1/users/me"

func (gmail) Kind() string { return "gmail" }

func (gmail) Describe() Kind {
	return Kind{
		Kind: "gmail",
		Name: "Gmail",
		Help: "Pulls mail threads (one note per thread; attachments are listed, not " +
			"ingested) from the labels you name, newest activity first. Mail is other " +
			"people's text: it stays untrusted unless the connector is trust=own, and " +
			"even then only threads in which every message was sent by you.",
		SecretHelp: "An OAuth token stored by `grimoire connect google` (read-only " +
			"gmail.readonly scope unless you opt in to drafting).",
		Fields: []Field{
			{Name: "labels", Label: "Labels", Placeholder: "INBOX, SENT",
				Help: "Comma-separated Gmail labels (system or your own). Default INBOX."},
			{Name: "since", Label: "Since", Placeholder: "2026-01-01",
				Help: "First sync starts here (YYYY-MM-DD). Default: the last 30 days."},
			{Name: "query", Label: "Extra Gmail search", Placeholder: "-category:promotions",
				Help: "Added to the search with AND, e.g. to drop promotions."},
		},
		DefaultPrefix: "connectors/mail/gmail",
	}
}

type gmailThread struct {
	ID       string `json:"id"`
	Snippet  string `json:"snippet"`
	Messages []struct {
		ID           string    `json:"id"`
		LabelIDs     []string  `json:"labelIds"`
		InternalDate string    `json:"internalDate"`
		Payload      gmailPart `json:"payload"`
	} `json:"messages"`
}

type gmailPart struct {
	MimeType string `json:"mimeType"`
	Filename string `json:"filename"`
	Headers  []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"headers"`
	Body struct {
		Data string `json:"data"`
		Size int    `json:"size"`
	} `json:"body"`
	Parts []gmailPart `json:"parts"`
}

func (p gmailPart) header(name string) string {
	for _, h := range p.Headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value
		}
	}
	return ""
}

// text returns the best text body (plain preferred over html) and attachments.
func (p gmailPart) text() (plain, html string, atts []string) {
	if p.Filename != "" {
		return "", "", []string{fmt.Sprintf("%s (%s, %d bytes)", p.Filename, p.MimeType, p.Body.Size)}
	}
	if len(p.Parts) == 0 {
		raw, _ := base64.URLEncoding.DecodeString(padB64(p.Body.Data))
		switch {
		case strings.HasPrefix(p.MimeType, "text/plain"):
			return string(raw), "", nil
		case strings.HasPrefix(p.MimeType, "text/html"):
			return "", string(raw), nil
		}
		return "", "", nil
	}
	for _, c := range p.Parts {
		pl, h, a := c.text()
		if plain == "" {
			plain = pl
		}
		if html == "" {
			html = h
		}
		atts = append(atts, a...)
	}
	return plain, html, atts
}

func padB64(s string) string {
	if m := len(s) % 4; m != 0 {
		s += strings.Repeat("=", 4-m)
	}
	return s
}

func gmailAddrs(h string) []string {
	if strings.TrimSpace(h) == "" {
		return nil
	}
	list, err := mail.ParseAddressList(h)
	if err != nil {
		return []string{strings.TrimSpace(h)}
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, a.String())
	}
	return out
}

// thread converts the API shape into the shared document.
func (t gmailThread) document() (Document, int64) {
	var msgs []MailMessage
	var newest int64
	labels := map[string]bool{}
	for _, m := range t.Messages {
		ms, _ := strconv.ParseInt(m.InternalDate, 10, 64)
		if ms > newest {
			newest = ms
		}
		plain, html, atts := m.Payload.text()
		body := strings.TrimSpace(plain)
		if body == "" {
			body = mailText("text/html", html)
		}
		from := gmailAddrs(m.Payload.header("From"))
		mm := MailMessage{
			ID: m.ID, Subject: m.Payload.header("Subject"), Body: body,
			To: gmailAddrs(m.Payload.header("To")), Cc: gmailAddrs(m.Payload.header("Cc")),
			Date: rfc3339(time.UnixMilli(ms)), Attachments: atts,
		}
		if len(from) > 0 {
			mm.From = from[0]
		}
		for _, l := range m.LabelIDs {
			labels[l] = true
			if l == "SENT" {
				mm.Sent = true
			}
		}
		msgs = append(msgs, mm)
	}
	doc := ThreadDocument(msgs)
	doc.ExternalID = t.ID
	doc.URL = "https://mail.google.com/mail/u/0/#all/" + t.ID
	doc.Meta["source"] = "gmail"
	names := make([]string, 0, len(labels))
	for l := range labels {
		names = append(names, l)
	}
	sort.Strings(names)
	doc.Meta["labels"] = strings.Join(names, ", ")
	return doc, newest
}

func (g gmail) get(ctx context.Context, in Input, path string, q url.Values, out any) error {
	req, err := jsonRequest(gmailBase+path, q, map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return err
	}
	return getJSON(ctx, in.Client, req, out)
}

func (g gmail) query(in Input) string {
	var parts []string
	var labels []string
	for _, l := range splitListLoose(in.Config.Get("labels"), "INBOX") {
		labels = append(labels, "label:"+strings.ReplaceAll(strings.ToLower(l), " ", "-"))
	}
	parts = append(parts, "("+strings.Join(labels, " OR ")+")")
	if q := in.Config.Get("query"); q != "" {
		parts = append(parts, "("+q+")")
	}
	return strings.Join(parts, " ")
}

// splitListLoose splits on commas only (labels may contain spaces).
func splitListLoose(s, def string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 && def != "" {
		return []string{def}
	}
	return out
}

func (g gmail) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("an OAuth token (run `grimoire connect google`)")
	}
	after := in.Cursor
	if after == "" {
		since, err := sinceCutoff(in.Config, 30)
		if err != nil {
			return Page{}, err
		}
		t, _ := time.Parse(time.RFC3339, since)
		after = strconv.FormatInt(t.Unix(), 10)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 25
	}
	q := g.query(in) + " after:" + after

	// Gmail lists newest-first. A cursor can only advance safely over the
	// OLDEST activity, so list every id (cheap) and fetch from the far end.
	var ids []string
	pageToken := ""
	for len(ids) < 2000 {
		var list struct {
			Threads []struct {
				ID string `json:"id"`
			} `json:"threads"`
			NextPageToken string `json:"nextPageToken"`
		}
		v := url.Values{"q": {q}, "maxResults": {"500"}}
		if pageToken != "" {
			v.Set("pageToken", pageToken)
		}
		if err := g.get(ctx, in, "/threads", v, &list); err != nil {
			return Page{}, err
		}
		for _, t := range list.Threads {
			ids = append(ids, t.ID)
		}
		if pageToken = list.NextPageToken; pageToken == "" {
			break
		}
	}
	for i, j := 0, len(ids)-1; i < j; i, j = i+1, j-1 {
		ids[i], ids[j] = ids[j], ids[i]
	}
	more := len(ids) > limit
	if more {
		ids = ids[:limit]
	}

	page := Page{Cursor: in.Cursor, More: more}
	var cursor int64
	if n, err := strconv.ParseInt(after, 10, 64); err == nil {
		cursor = n
	}
	for _, id := range ids {
		var th gmailThread
		if err := g.get(ctx, in, "/threads/"+url.PathEscape(id), url.Values{"format": {"full"}}, &th); err != nil {
			return Page{}, err
		}
		doc, newest := th.document()
		page.Docs = append(page.Docs, doc)
		if s := newest / 1000; s > cursor {
			cursor = s
		}
	}
	page.Cursor = strconv.FormatInt(cursor, 10)
	return page, nil
}

// ---- live

func (g gmail) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	var list struct {
		Threads []struct {
			ID      string `json:"id"`
			Snippet string `json:"snippet"`
		} `json:"threads"`
	}
	if err := g.get(ctx, in, "/threads", url.Values{"q": {query}, "maxResults": {strconv.Itoa(limit)}}, &list); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, t := range list.Threads {
		var th gmailThread
		if err := g.get(ctx, in, "/threads/"+url.PathEscape(t.ID),
			url.Values{"format": {"metadata"}, "metadataHeaders": {"Subject", "From", "Date"}}, &th); err != nil {
			return nil, err
		}
		h := Hit{ID: t.ID, Snippet: t.Snippet, URL: "https://mail.google.com/mail/u/0/#all/" + t.ID}
		if len(th.Messages) > 0 {
			last := th.Messages[len(th.Messages)-1]
			h.Title = th.Messages[0].Payload.header("Subject")
			h.Author = last.Payload.header("From")
			h.Updated = last.Payload.header("Date")
		}
		hits = append(hits, h)
	}
	return hits, nil
}

func (g gmail) Read(ctx context.Context, in Input, id string) (Item, error) {
	var th gmailThread
	if err := g.get(ctx, in, "/threads/"+url.PathEscape(id), url.Values{"format": {"full"}}, &th); err != nil {
		return Item{}, err
	}
	doc, _ := th.document()
	item := Item{ID: id, Title: doc.Title, Body: doc.Body, URL: doc.URL, Updated: doc.Updated, Author: doc.Author}
	for _, m := range th.Messages {
		_, _, atts := m.Payload.text()
		item.Attachments = append(item.Attachments, atts...)
	}
	return item, nil
}

// ---- actions

func (gmail) Actions() []ActionSpec {
	fields := []Field{
		{Name: "to", Label: "To", Required: true},
		{Name: "subject", Label: "Subject", Required: true},
		{Name: "body", Label: "Body", Required: true},
		{Name: "cc", Label: "Cc"},
	}
	return []ActionSpec{
		{Name: "create_draft", Summary: "Create a Gmail draft (never sent; the owner reviews and sends it)",
			Params: fields, Scopes: []string{"https://www.googleapis.com/auth/gmail.compose"}},
		{Name: "send_message", Summary: "SEND an email immediately. Separate opt-in from drafting.",
			Params: fields, Scopes: []string{"https://www.googleapis.com/auth/gmail.send"}, Irreversible: true},
	}
}

func (g gmail) Act(ctx context.Context, in Input, action string, p map[string]string) (ActionResult, error) {
	if action != "create_draft" && action != "send_message" {
		return ActionResult{}, fmt.Errorf("gmail has no action %q", action)
	}
	raw, err := rfc822(p)
	if err != nil {
		return ActionResult{}, err
	}
	enc := base64.URLEncoding.EncodeToString([]byte(raw))
	if action == "create_draft" {
		var out struct {
			ID      string `json:"id"`
			Message struct {
				ID       string `json:"id"`
				ThreadID string `json:"threadId"`
			} `json:"message"`
		}
		err := postJSON(ctx, in, gmailBase+"/drafts", map[string]any{"message": map[string]any{"raw": enc}}, &out)
		if err != nil {
			return ActionResult{}, err
		}
		return ActionResult{ID: out.ID, URL: "https://mail.google.com/mail/u/0/#drafts",
			Message: "draft created; it has not been sent"}, nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := postJSON(ctx, in, gmailBase+"/messages/send", map[string]any{"raw": enc}, &out); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{ID: out.ID, Message: "message sent"}, nil
}

// rfc822 builds a plain-text message. Header values are rejected, not
// escaped, if they contain a line break: a newline in "subject" is how a
// crafted argument would smuggle in a Bcc.
func rfc822(p map[string]string) (string, error) {
	for _, k := range []string{"to", "cc", "subject"} {
		if strings.ContainsAny(p[k], "\r\n") {
			return "", fmt.Errorf("%s must not contain line breaks", k)
		}
	}
	if strings.TrimSpace(p["to"]) == "" {
		return "", fmt.Errorf("to is required")
	}
	var b strings.Builder
	b.WriteString("To: " + p["to"] + "\r\n")
	if p["cc"] != "" {
		b.WriteString("Cc: " + p["cc"] + "\r\n")
	}
	b.WriteString("Subject: " + mimeHeader(p["subject"]) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\n")
	b.WriteString(p["body"])
	return b.String(), nil
}
