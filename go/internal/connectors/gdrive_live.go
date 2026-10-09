package connectors

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
)

// Live search, read and create for Google Drive. The sync side is google.go.

const driveBase = "https://www.googleapis.com/drive/v3"

// escapeDriveQ escapes a value for a Drive query string literal.
func escapeDriveQ(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`)
}

func (g gdrive) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	clauses := []string{"trashed = false", "fullText contains '" + escapeDriveQ(query) + "'"}
	if folder := in.Config.Get("folder"); folder != "" {
		clauses = append(clauses, fmt.Sprintf("'%s' in parents", escapeDriveQ(folder)))
	}
	q := url.Values{
		"q": {strings.Join(clauses, " and ")}, "pageSize": {strconv.Itoa(limit)},
		"orderBy":           {"modifiedTime desc"},
		"fields":            {"files(id,name,mimeType,modifiedTime,webViewLink,owners(displayName))"},
		"supportsAllDrives": {"true"}, "includeItemsFromAllDrives": {"true"},
	}
	req, err := jsonRequest(driveBase+"/files", q, map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return nil, err
	}
	var out struct {
		Files []struct {
			ID, Name, MimeType, ModifiedTime, WebViewLink string
			Owners                                        []struct{ DisplayName string }
		} `json:"files"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, f := range out.Files {
		h := Hit{ID: f.ID, Title: f.Name, URL: f.WebViewLink, Updated: f.ModifiedTime, Snippet: f.MimeType}
		if len(f.Owners) > 0 {
			h.Author = f.Owners[0].DisplayName
		}
		hits = append(hits, h)
	}
	return hits, nil
}

func (g gdrive) Read(ctx context.Context, in Input, id string) (Item, error) {
	req, err := jsonRequest(driveBase+"/files/"+url.PathEscape(id),
		url.Values{"fields": {"id,name,mimeType,modifiedTime,webViewLink"}, "supportsAllDrives": {"true"}},
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return Item{}, err
	}
	var f struct {
		ID, Name, MimeType, ModifiedTime, WebViewLink string
	}
	if err := getJSON(ctx, in.Client, req, &f); err != nil {
		return Item{}, err
	}
	body, err := g.content(ctx, in, f.ID, f.MimeType)
	if err != nil {
		return Item{}, err
	}
	return Item{ID: f.ID, Title: f.Name, Body: body, URL: f.WebViewLink, Updated: f.ModifiedTime}, nil
}

func (gdrive) Actions() []ActionSpec {
	return []ActionSpec{{
		Name:    "create_doc",
		Summary: "Create a new Google Doc from plain text. Never edits or deletes existing files.",
		Params: []Field{
			{Name: "title", Label: "Title", Required: true},
			{Name: "body", Label: "Text", Required: true},
			{Name: "folder_id", Label: "Folder ID (defaults to the connector's folder)"},
		},
		// drive.file only reaches files this app created: it cannot read or
		// change the rest of the drive.
		Scopes: []string{"https://www.googleapis.com/auth/drive.file"},
	}}
}

func (g gdrive) Act(ctx context.Context, in Input, action string, p map[string]string) (ActionResult, error) {
	if action != "create_doc" {
		return ActionResult{}, fmt.Errorf("gdrive has no action %q", action)
	}
	meta := map[string]any{"name": p["title"], "mimeType": "application/vnd.google-apps.document"}
	if folder := firstNonEmpty(p["folder_id"], in.Config.Get("folder")); folder != "" {
		meta["parents"] = []string{folder}
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := textproto.MIMEHeader{"Content-Type": {"application/json; charset=UTF-8"}}
	part, _ := mw.CreatePart(h)
	mj, _ := jsonMarshal(meta)
	part.Write(mj)
	h = textproto.MIMEHeader{"Content-Type": {"text/plain; charset=UTF-8"}}
	part, _ = mw.CreatePart(h)
	part.Write([]byte(p["body"]))
	mw.Close()

	u := "https://www.googleapis.com/upload/drive/v3/files?uploadType=multipart&supportsAllDrives=true&fields=id,webViewLink"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, &buf)
	if err != nil {
		return ActionResult{}, err
	}
	req.Header.Set("Authorization", "Bearer "+in.Secret)
	req.Header.Set("Content-Type", "multipart/related; boundary="+mw.Boundary())
	var out struct {
		ID          string `json:"id"`
		WebViewLink string `json:"webViewLink"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return ActionResult{}, err
	}
	return ActionResult{ID: out.ID, URL: out.WebViewLink, Message: "document created"}, nil
}
