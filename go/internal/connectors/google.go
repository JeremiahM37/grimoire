package connectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Google Drive. Documents are exported as plain text rather than downloaded:
// a .docx is not something to embed, and Drive will do the conversion.

func init() { Register(gdrive{}) }

type gdrive struct{}

func (gdrive) Kind() string { return "gdrive" }

func (gdrive) Describe() Kind {
	return Kind{
		Kind: "gdrive",
		Name: "Google Drive",
		Help: "Pulls Docs (and any plain-text or markdown files) that changed since " +
			"the last sync. Restrict it to a folder unless you mean the whole drive.",
		SecretHelp: "An OAuth access token with drive.readonly scope, or a service " +
			"account token. Service accounts see only what has been shared with them, " +
			"which is usually what you want: share the folder with the account's email.",
		Fields: []Field{
			{Name: "folder", Label: "Folder ID",
				Placeholder: "1AbC…",
				Help: "From the folder's URL. Leave empty to sync everything the " +
					"credential can read."},
			{Name: "mime", Label: "File types",
				Placeholder: "documents",
				Help:        "documents (default) or all"},
		},
		DefaultPrefix: "connectors/drive",
	}
}

func (g gdrive) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("an access token")
	}
	clauses := []string{"trashed = false"}
	if folder := in.Config.Get("folder"); folder != "" {
		clauses = append(clauses, fmt.Sprintf("%q in parents", folder))
	}
	if !strings.EqualFold(in.Config.Get("mime"), "all") {
		clauses = append(clauses,
			"(mimeType = 'application/vnd.google-apps.document' or mimeType = 'text/plain' or mimeType = 'text/markdown')")
	}
	if in.Cursor != "" {
		clauses = append(clauses, fmt.Sprintf("modifiedTime > %q", in.Cursor))
	}

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{
		"q":        {strings.Join(clauses, " and ")},
		"orderBy":  {"modifiedTime"},
		"pageSize": {strconv.Itoa(limit)},
		"fields":   {"files(id,name,mimeType,modifiedTime,webViewLink,owners(displayName))"},
		// A drive that lives in a shared drive returns nothing without these,
		// which reads as "the folder is empty" rather than as a missing flag.
		"supportsAllDrives":         {"true"},
		"includeItemsFromAllDrives": {"true"},
	}
	req, err := jsonRequest("https://www.googleapis.com/drive/v3/files", q,
		map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return Page{}, err
	}
	var out struct {
		Files []struct {
			ID           string `json:"id"`
			Name         string `json:"name"`
			MimeType     string `json:"mimeType"`
			ModifiedTime string `json:"modifiedTime"`
			WebViewLink  string `json:"webViewLink"`
			Owners       []struct {
				DisplayName string `json:"displayName"`
			} `json:"owners"`
		} `json:"files"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}

	page := Page{Cursor: in.Cursor, More: len(out.Files) >= limit}
	for _, f := range out.Files {
		body, err := g.content(ctx, in, f.ID, f.MimeType)
		if err != nil {
			// One unreadable file must not stop the sync: record it in place
			// of the body so the failure is visible in the note itself.
			body = "> could not read this file: " + err.Error()
		}
		author := ""
		if len(f.Owners) > 0 {
			author = f.Owners[0].DisplayName
		}
		page.Docs = append(page.Docs, Document{
			ExternalID: f.ID,
			Title:      f.Name,
			Body:       body,
			URL:        f.WebViewLink,
			Updated:    f.ModifiedTime,
			Author:     author,
			Meta:       map[string]string{"mime": f.MimeType, "source": "google-drive"},
		})
		if f.ModifiedTime > page.Cursor {
			page.Cursor = f.ModifiedTime
		}
	}
	return page, nil
}

// content fetches a file's text. Google-native documents are exported;
// anything else is downloaded as-is.
func (g gdrive) content(ctx context.Context, in Input, id, mime string) (string, error) {
	var rawURL string
	var q url.Values
	if strings.HasPrefix(mime, "application/vnd.google-apps") {
		rawURL = "https://www.googleapis.com/drive/v3/files/" + url.PathEscape(id) + "/export"
		q = url.Values{"mimeType": {"text/plain"}}
	} else {
		rawURL = "https://www.googleapis.com/drive/v3/files/" + url.PathEscape(id)
		q = url.Values{"alt": {"media"}, "supportsAllDrives": {"true"}}
	}
	req, err := jsonRequest(rawURL, q, map[string]string{"Authorization": "Bearer " + in.Secret})
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/plain")
	client := in.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 400 {
		return "", statusError(req, resp.StatusCode, body)
	}
	return strings.TrimSpace(string(body)), nil
}
