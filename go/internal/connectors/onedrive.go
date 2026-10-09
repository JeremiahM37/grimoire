package connectors

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// OneDrive via Microsoft Graph delta queries. The cursor is the opaque
// @odata.deltaLink (or, mid-enumeration, the next @odata.nextLink).

func init() { Register(onedrive{}) }

type onedrive struct{}

func (onedrive) Kind() string { return "onedrive" }

func (onedrive) Describe() Kind {
	return Kind{
		Kind: "onedrive",
		Name: "OneDrive (Microsoft 365)",
		Help: "Pulls files that changed since the last sync. Text-like files (text/*, .md, .txt, " +
			".csv, .json) are downloaded; Office, PDF and image files are listed with metadata only.",
		SecretHelp: "A Microsoft Graph OAuth token, stored by `grimoire connect microsoft` " +
			"as a vault secret holding JSON; connectors accept that secret directly. " +
			"Scopes are read-only by default (Mail.Read, Files.Read, User.Read, " +
			"offline_access); Mail.ReadWrite is needed only if the Outlook create_draft action is enabled.",
		Fields: []Field{
			{Name: "folder", Label: "Folder", Placeholder: "Documents/Notes",
				Help: "Path within the drive. Empty syncs the whole drive."},
		},
		DefaultPrefix: "connectors/onedrive",
	}
}

const (
	onedriveMaxContent = 200 << 10
	onedriveNoExtract  = "> Content not extracted: Office/PDF files are listed, not converted."
)

type driveUser struct {
	User struct {
		ID          string `json:"id"`
		DisplayName string `json:"displayName"`
	} `json:"user"`
}

type driveItem struct {
	ID                   string    `json:"id"`
	Name                 string    `json:"name"`
	Size                 int64     `json:"size"`
	WebURL               string    `json:"webUrl"`
	LastModifiedDateTime string    `json:"lastModifiedDateTime"`
	CreatedBy            driveUser `json:"createdBy"`
	LastModifiedBy       driveUser `json:"lastModifiedBy"`
	File                 *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
	Folder  *struct{} `json:"folder"`
	Deleted *struct{} `json:"deleted"`
}

func (d onedrive) deltaURL(cfg Config) string {
	folder := strings.Trim(cfg.Get("folder"), "/")
	if folder == "" {
		return graphBase + "/me/drive/root/delta"
	}
	segs := strings.Split(folder, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return graphBase + "/me/drive/root:/" + strings.Join(segs, "/") + ":/delta"
}

func (d onedrive) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, missing("an access token")
	}
	next := in.Cursor
	if next == "" {
		next = d.deltaURL(in.Config)
	} else if err := checkGraphHost(next); err != nil {
		return Page{}, err
	}
	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	var me struct {
		ID string `json:"id"`
	}
	if err := graphGet(ctx, in, graphBase+"/me", url.Values{"$select": {"id"}}, &me); err != nil {
		return Page{}, err
	}

	page := Page{Cursor: in.Cursor}
	count := 0
	for next != "" {
		var out struct {
			Value     []driveItem `json:"value"`
			NextLink  string      `json:"@odata.nextLink"`
			DeltaLink string      `json:"@odata.deltaLink"`
		}
		if err := graphGetLink(ctx, in, next, &out); err != nil {
			return Page{}, err
		}
		for _, it := range out.Value {
			// Deleted items are skipped, not reaped: deletion is handled by
			// never setting Page.Complete, so a removed file's note stays.
			if it.Deleted != nil || it.Folder != nil || it.File == nil {
				continue
			}
			body := d.body(ctx, in, it)
			uid := it.CreatedBy.User.ID
			page.Docs = append(page.Docs, Document{
				ExternalID: "onedrive:" + it.ID,
				Title:      it.Name,
				Body:       body,
				URL:        it.WebURL,
				Updated:    it.LastModifiedDateTime,
				Author:     it.LastModifiedBy.User.DisplayName,
				Own:        me.ID != "" && uid == me.ID && it.LastModifiedBy.User.ID == me.ID,
				Meta:       map[string]string{"source": "onedrive", "mime": it.File.MimeType},
			})
			count++
		}
		switch {
		case out.NextLink != "":
			next = out.NextLink
			if count >= limit {
				page.Cursor, page.More = next, true
				return page, nil
			}
		case out.DeltaLink != "":
			page.Cursor = out.DeltaLink
			return page, nil
		default:
			return page, nil
		}
	}
	return page, nil
}

func textLike(name, mime string) bool {
	if strings.HasPrefix(strings.ToLower(mime), "text/") {
		return true
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".md", ".txt", ".csv", ".json":
		return true
	}
	return false
}

// body returns the note text for one file: downloaded content for text-like
// files, otherwise a metadata-only listing.
func (d onedrive) body(ctx context.Context, in Input, it driveItem) string {
	mime := ""
	if it.File != nil {
		mime = it.File.MimeType
	}
	if textLike(it.Name, mime) {
		text, err := d.download(ctx, in, it.ID)
		if err != nil {
			return "> could not read this file: " + err.Error()
		}
		return text
	}
	return fmt.Sprintf("%s\n\n- name: %s\n- size: %d bytes\n- type: %s\n- link: %s",
		onedriveNoExtract, it.Name, it.Size, mime, it.WebURL)
}

func (d onedrive) download(ctx context.Context, in Input, id string) (string, error) {
	req, err := graphRequest(http.MethodGet, graphBase+"/me/drive/items/"+url.PathEscape(id)+"/content", nil, in.Secret, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "*/*")
	client := in.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	resp, err := client.Do(req.WithContext(ctx))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", statusError(req, resp.StatusCode, b)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, onedriveMaxContent+1))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if len(b) > onedriveMaxContent {
		s = strings.TrimSpace(string(b[:onedriveMaxContent])) + "\n\n[… truncated]"
	}
	return s, nil
}

func (d onedrive) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if in.Secret == "" {
		return nil, missing("an access token")
	}
	if strings.ContainsAny(query, "\r\n") || strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("%w: query must be a single non-empty line", ErrConfig)
	}
	esc := url.PathEscape(strings.ReplaceAll(strings.TrimSpace(query), "'", "''"))
	var out struct {
		Value []driveItem `json:"value"`
	}
	err := graphGet(ctx, in, graphBase+"/me/drive/root/search(q='"+esc+"')",
		url.Values{"$top": {strconv.Itoa(clampTop(limit))}}, &out)
	if err != nil {
		return nil, err
	}
	var hits []Hit
	for _, it := range out.Value {
		if it.Folder != nil {
			continue
		}
		snip := ""
		if it.File != nil {
			snip = fmt.Sprintf("%s, %d bytes", it.File.MimeType, it.Size)
		}
		hits = append(hits, Hit{ID: it.ID, Title: it.Name, Snippet: snip, URL: it.WebURL,
			Updated: it.LastModifiedDateTime, Author: it.LastModifiedBy.User.DisplayName})
	}
	return hits, nil
}

func (d onedrive) Read(ctx context.Context, in Input, id string) (Item, error) {
	if in.Secret == "" {
		return Item{}, missing("an access token")
	}
	id = strings.TrimPrefix(strings.TrimSpace(id), "onedrive:")
	if id == "" {
		return Item{}, missing("an id")
	}
	var it driveItem
	if err := graphGet(ctx, in, graphBase+"/me/drive/items/"+url.PathEscape(id), nil, &it); err != nil {
		return Item{}, err
	}
	if it.File == nil {
		return Item{}, fmt.Errorf("onedrive: %q is not a file", id)
	}
	return Item{ID: it.ID, Title: it.Name, Body: d.body(ctx, in, it), URL: it.WebURL,
		Updated: it.LastModifiedDateTime, Author: it.LastModifiedBy.User.DisplayName}, nil
}
