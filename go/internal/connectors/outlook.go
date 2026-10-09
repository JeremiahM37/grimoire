package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Outlook mail via Microsoft Graph. Like Gmail and IMAP, the unit is a whole
// conversation: every message in a touched conversation is re-fetched so the
// note is always the complete thread, and attachments are listed, never
// downloaded. The secret is an OAuth access token; refresh is handled
// elsewhere.

const graphBase = "https://graph.microsoft.com/v1.0"

func init() { Register(outlook{}) }

type outlook struct{}

func (outlook) Kind() string { return "outlook" }

func (outlook) Describe() Kind {
	return Kind{
		Kind: "outlook",
		Name: "Outlook mail (Microsoft 365)",
		Help: "Pulls mail from the chosen folders as one note per conversation " +
			"(whole thread, attachments listed but never downloaded). Drafts are skipped. " +
			"With the create_draft action enabled an agent may leave a draft in Drafts; " +
			"nothing is ever sent.",
		SecretHelp: "A Microsoft Graph OAuth token, stored by `grimoire connect microsoft` " +
			"as a vault secret holding JSON; connectors accept that secret directly. " +
			"Scopes are read-only by default (Mail.Read, Files.Read, User.Read, " +
			"offline_access); Mail.ReadWrite is needed only if create_draft is enabled.",
		Fields: []Field{
			{Name: "folders", Label: "Folders", Placeholder: "inbox,sentitems",
				Help: "Comma-separated well-known folder names (inbox, sentitems, archive…) or folder ids. Default inbox,sentitems."},
			{Name: "since", Label: "Since", Placeholder: "2026-01-01",
				Help: "First sync starts here (YYYY-MM-DD). Default: 30 days back."},
		},
		DefaultPrefix: "connectors/mail/outlook",
	}
}

const outlookSelect = "id,conversationId,subject,from,toRecipients,ccRecipients,receivedDateTime,body,hasAttachments,webLink,isDraft"

type graphAddr struct {
	EmailAddress struct {
		Name    string `json:"name"`
		Address string `json:"address"`
	} `json:"emailAddress"`
}

func (a graphAddr) display() string {
	e := a.EmailAddress
	switch {
	case e.Name != "" && e.Address != "" && e.Name != e.Address:
		return e.Name + " <" + e.Address + ">"
	case e.Address != "":
		return e.Address
	}
	return e.Name
}

type graphMessage struct {
	ID               string      `json:"id"`
	ConversationID   string      `json:"conversationId"`
	Subject          string      `json:"subject"`
	From             graphAddr   `json:"from"`
	To               []graphAddr `json:"toRecipients"`
	Cc               []graphAddr `json:"ccRecipients"`
	ReceivedDateTime string      `json:"receivedDateTime"`
	BodyPreview      string      `json:"bodyPreview"`
	Body             struct {
		ContentType string `json:"contentType"`
		Content     string `json:"content"`
	} `json:"body"`
	WebLink     string `json:"webLink"`
	IsDraft     bool   `json:"isDraft"`
	Attachments []struct {
		Name        string `json:"name"`
		ContentType string `json:"contentType"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
}

type graphMessages struct {
	Value    []graphMessage `json:"value"`
	NextLink string         `json:"@odata.nextLink"`
}

// graphRequest builds a Graph request. A "+" in the query is rewritten to %20
// because Graph's OData parser does not treat it as a space.
func graphRequest(method, rawURL string, q url.Values, token string, body []byte) (*http.Request, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if len(q) > 0 {
		u.RawQuery = strings.ReplaceAll(q.Encode(), "+", "%20")
	}
	var rd *bytes.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	var req *http.Request
	if rd != nil {
		req, err = http.NewRequest(method, u.String(), rd)
	} else {
		req, err = http.NewRequest(method, u.String(), nil)
	}
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "grimoire-connector")
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func graphGet(ctx context.Context, in Input, rawURL string, q url.Values, out any) error {
	req, err := graphRequest(http.MethodGet, rawURL, q, in.Secret, nil)
	if err != nil {
		return err
	}
	return getJSON(ctx, in.Client, req, out)
}

// mailbox returns the lowercase addresses that identify the signed-in user.
func mailbox(ctx context.Context, in Input) (map[string]bool, error) {
	var me struct {
		Mail string `json:"mail"`
		UPN  string `json:"userPrincipalName"`
	}
	if err := graphGet(ctx, in, graphBase+"/me", url.Values{"$select": {"mail,userPrincipalName"}}, &me); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, a := range []string{me.Mail, me.UPN} {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			out[a] = true
		}
	}
	return out, nil
}

func (m graphMessage) toMail(own map[string]bool) MailMessage {
	mm := MailMessage{
		ID:       m.ID,
		From:     m.From.display(),
		FromAddr: strings.ToLower(strings.TrimSpace(m.From.EmailAddress.Address)),
		Date:     m.ReceivedDateTime,
		Subject:  m.Subject,
		Body:     mailText(m.Body.ContentType, m.Body.Content),
	}
	for _, a := range m.To {
		mm.To = append(mm.To, a.display())
	}
	for _, a := range m.Cc {
		mm.Cc = append(mm.Cc, a.display())
	}
	for _, a := range m.Attachments {
		mm.Attachments = append(mm.Attachments, fmt.Sprintf("%s (%s, %d bytes)", a.Name, a.ContentType, a.Size))
	}
	mm.Sent = mm.FromAddr != "" && own[mm.FromAddr]
	return mm
}

// conversation fetches every non-draft message of one conversation.
func (o outlook) conversation(ctx context.Context, in Input, convID string) ([]graphMessage, error) {
	q := url.Values{
		"$filter": {"conversationId eq '" + strings.ReplaceAll(convID, "'", "''") + "'"},
		"$select": {outlookSelect},
		"$expand": {"attachments($select=name,contentType,size)"},
		"$top":    {"50"},
	}
	var all []graphMessage
	next := graphBase + "/me/messages"
	for pages := 0; next != "" && pages < 10; pages++ {
		var out graphMessages
		var err error
		if pages == 0 {
			err = graphGet(ctx, in, next, q, &out)
		} else {
			err = graphGetLink(ctx, in, next, &out)
		}
		if err != nil {
			return nil, err
		}
		for _, m := range out.Value {
			if !m.IsDraft {
				all = append(all, m)
			}
		}
		next = out.NextLink
	}
	return all, nil
}

// graphGetLink follows an @odata link, but only to Graph itself: the bearer
// token must not be sent to a host a response named.
func graphGetLink(ctx context.Context, in Input, link string, out any) error {
	if err := checkGraphHost(link); err != nil {
		return err
	}
	req, err := graphRequest(http.MethodGet, link, nil, in.Secret, nil)
	if err != nil {
		return err
	}
	return getJSON(ctx, in.Client, req, out)
}

func checkGraphHost(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host != "graph.microsoft.com" || u.Scheme != "https" {
		return fmt.Errorf("%w: link is not on graph.microsoft.com", ErrConfig)
	}
	return nil
}

func (o outlook) document(msgs []graphMessage, own map[string]bool) Document {
	mm := make([]MailMessage, 0, len(msgs))
	for _, m := range msgs {
		mm = append(mm, m.toMail(own))
	}
	doc := ThreadDocument(mm)
	// URL = webLink of the latest message.
	latest := msgs[0]
	for _, m := range msgs {
		if m.ReceivedDateTime >= latest.ReceivedDateTime {
			latest = m
		}
	}
	doc.URL = latest.WebLink
	doc.Meta["source"] = "outlook"
	return doc
}

func (o outlook) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("an access token")
	}
	since := in.Cursor
	if since == "" {
		var err error
		if since, err = sinceCutoff(in.Config, 30); err != nil {
			return Page{}, err
		}
	}
	folders := splitList(in.Config.Get("folders"))
	if len(folders) == 0 {
		folders = []string{"inbox", "sentitems"}
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	own, err := mailbox(ctx, in)
	if err != nil {
		return Page{}, err
	}

	type stub struct{ conv, date string }
	var found []stub
	more := false
	for _, folder := range folders {
		q := url.Values{
			"$filter":  {"receivedDateTime gt " + since},
			"$orderby": {"receivedDateTime asc"},
			"$top":     {strconv.Itoa(limit)},
			"$select":  {outlookSelect},
			"$expand":  {"attachments($select=name,contentType,size)"},
		}
		next := graphBase + "/me/mailFolders/" + url.PathEscape(folder) + "/messages"
		count := 0
		for pages := 0; next != ""; pages++ {
			var out graphMessages
			if pages == 0 {
				err = graphGet(ctx, in, next, q, &out)
			} else {
				err = graphGetLink(ctx, in, next, &out)
			}
			if err != nil {
				return Page{}, err
			}
			for _, m := range out.Value {
				count++
				if m.IsDraft || m.ConversationID == "" {
					// Still advances the cursor below via found only if real;
					// drafts are never handled, so they are re-skipped.
					continue
				}
				found = append(found, stub{m.ConversationID, m.ReceivedDateTime})
			}
			next = out.NextLink
			if count >= limit {
				if next != "" {
					more = true
				}
				break
			}
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].date < found[j].date })
	if len(found) > limit {
		cut := found[limit-1].date
		end := limit
		for end < len(found) && found[end].date == cut {
			end++
		}
		if end < len(found) {
			more = true
		}
		found = found[:end]
	}

	page := Page{Cursor: since, More: more}
	if in.Cursor == "" && len(found) == 0 {
		page.Cursor = in.Cursor
	}
	seen := map[string]bool{}
	for _, s := range found {
		if s.date > page.Cursor {
			page.Cursor = s.date
		}
		if seen[s.conv] {
			continue
		}
		seen[s.conv] = true
		msgs, err := o.conversation(ctx, in, s.conv)
		if err != nil {
			return Page{}, err
		}
		if len(msgs) == 0 {
			continue
		}
		doc := o.document(msgs, own)
		doc.ExternalID = "outlook:" + s.conv
		page.Docs = append(page.Docs, doc)
	}
	return page, nil
}

func clampTop(limit int) int {
	if limit <= 0 || limit > 50 {
		return 10
	}
	return limit
}

func (o outlook) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if in.Secret == "" {
		return nil, missing("an access token")
	}
	if strings.ContainsAny(query, "\r\n") || strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query must be a single non-empty line", ErrConfig)
	}
	esc := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(strings.TrimSpace(query))
	q := url.Values{
		"$search": {`"` + esc + `"`},
		"$top":    {strconv.Itoa(clampTop(limit))},
		"$select": {"id,conversationId,subject,from,receivedDateTime,bodyPreview,webLink"},
	}
	var out graphMessages
	if err := graphGet(ctx, in, graphBase+"/me/messages", q, &out); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, m := range out.Value {
		hits = append(hits, Hit{
			ID:      firstNonEmpty(m.ConversationID, m.ID),
			Title:   firstNonEmpty(m.Subject, "(no subject)"),
			Snippet: strings.TrimSpace(m.BodyPreview),
			URL:     m.WebLink,
			Updated: m.ReceivedDateTime,
			Author:  m.From.display(),
		})
	}
	return hits, nil
}

func (o outlook) Read(ctx context.Context, in Input, id string) (Item, error) {
	if in.Secret == "" {
		return Item{}, missing("an access token")
	}
	id = strings.TrimPrefix(strings.TrimSpace(id), "outlook:")
	if id == "" {
		return Item{}, missing("an id")
	}
	own, err := mailbox(ctx, in)
	if err != nil {
		return Item{}, err
	}
	msgs, err := o.conversation(ctx, in, id)
	if err != nil {
		return Item{}, err
	}
	if len(msgs) == 0 {
		// The id may be a single message id rather than a conversation id.
		var m graphMessage
		q := url.Values{"$select": {outlookSelect}, "$expand": {"attachments($select=name,contentType,size)"}}
		if err := graphGet(ctx, in, graphBase+"/me/messages/"+url.PathEscape(id), q, &m); err != nil {
			return Item{}, err
		}
		if m.ConversationID != "" {
			if msgs, err = o.conversation(ctx, in, m.ConversationID); err != nil {
				return Item{}, err
			}
		}
		if len(msgs) == 0 && !m.IsDraft {
			msgs = []graphMessage{m}
		}
	}
	if len(msgs) == 0 {
		return Item{}, fmt.Errorf("outlook: nothing readable for %q", id)
	}
	doc := o.document(msgs, own)
	var atts []string
	for _, m := range msgs {
		atts = append(atts, m.toMail(own).Attachments...)
	}
	return Item{ID: id, Title: doc.Title, Body: doc.Body, URL: doc.URL,
		Updated: doc.Updated, Author: doc.Author, Attachments: atts}, nil
}

func (outlook) Actions() []ActionSpec {
	return []ActionSpec{{
		Name:    "create_draft",
		Summary: "Create a draft email in Drafts. Nothing is sent; the owner reviews and sends it.",
		Params: []Field{
			{Name: "to", Label: "To", Required: true, Help: "Comma-separated addresses"},
			{Name: "subject", Label: "Subject", Required: true},
			{Name: "body", Label: "Body", Required: true},
			{Name: "cc", Label: "Cc"},
		},
		Scopes: []string{"Mail.ReadWrite"},
	}}
}

func recipients(s string) ([]map[string]any, error) {
	var out []map[string]any
	for _, a := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if a = strings.TrimSpace(a); a != "" {
			out = append(out, map[string]any{"emailAddress": map[string]string{"address": a}})
		}
	}
	return out, nil
}

func (o outlook) Act(ctx context.Context, in Input, action string, params map[string]string) (ActionResult, error) {
	if action != "create_draft" {
		return ActionResult{}, fmt.Errorf("outlook: unknown action %q", action)
	}
	if in.Secret == "" {
		return ActionResult{}, missing("an access token")
	}
	for _, k := range []string{"to", "cc", "subject"} {
		if strings.ContainsAny(params[k], "\r\n") {
			return ActionResult{}, fmt.Errorf("outlook: %s must not contain line breaks", k)
		}
	}
	to, _ := recipients(params["to"])
	cc, _ := recipients(params["cc"])
	if len(to) == 0 {
		return ActionResult{}, missing("to")
	}
	if strings.TrimSpace(params["subject"]) == "" {
		return ActionResult{}, missing("subject")
	}
	payload := map[string]any{
		"subject":      params["subject"],
		"body":         map[string]string{"contentType": "Text", "content": params["body"]},
		"toRecipients": to,
	}
	if len(cc) > 0 {
		payload["ccRecipients"] = cc
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return ActionResult{}, err
	}
	req, err := graphRequest(http.MethodPost, graphBase+"/me/messages", nil, in.Secret, b)
	if err != nil {
		return ActionResult{}, err
	}
	var out struct {
		ID      string `json:"id"`
		WebLink string `json:"webLink"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{ID: out.ID, URL: out.WebLink, Message: "draft created in Drafts (not sent)"}, nil
}
