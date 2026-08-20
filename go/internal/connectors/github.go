package connectors

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// GitHub: issues and pull requests as documents, and optionally the repository's
// markdown files.
//
// Issues are where a project's reasoning ends up — why something was done this
// way, what was tried, what broke. The diff is in git; the argument is here.

func init() { Register(github{}) }

type github struct{}

func (github) Kind() string { return "github" }

func (github) Describe() Kind {
	return Kind{
		Kind: "github",
		Name: "GitHub",
		Help: "Pulls issues and pull requests (with their comments) from a repository, " +
			"and optionally its markdown files. Works with GitHub Enterprise via the " +
			"API URL field.",
		SecretHelp: "A personal access token with repo scope (or public_repo for " +
			"public repositories). Fine-grained tokens need Issues: read and Contents: read.",
		Fields: []Field{
			{Name: "repo", Label: "Repository", Required: true, Placeholder: "owner/name"},
			{Name: "api", Label: "API URL", Placeholder: "https://api.github.com",
				Help: "Change only for GitHub Enterprise."},
			{Name: "docs", Label: "Also pull markdown files", Placeholder: "no",
				Help: "yes pulls *.md from the default branch as well as issues."},
		},
		DefaultPrefix: "connectors/github",
	}
}

func (g github) Fetch(ctx context.Context, in Input) (Page, error) {
	repo := in.Config.Get("repo")
	if !strings.Contains(repo, "/") {
		return Page{}, fmt.Errorf("%w: repository must be owner/name", ErrConfig)
	}
	api := strings.TrimRight(in.Config.Get("api"), "/")
	if api == "" {
		api = "https://api.github.com"
	}
	headers := map[string]string{"Accept": "application/vnd.github+json"}
	if in.Secret != "" {
		headers["Authorization"] = "Bearer " + in.Secret
	}

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{
		"state":     {"all"},
		"sort":      {"updated"},
		"direction": {"asc"},
		"per_page":  {strconv.Itoa(limit)},
	}
	if in.Cursor != "" {
		q.Set("since", in.Cursor)
	}
	req, err := jsonRequest(api+"/repos/"+repo+"/issues", q, headers)
	if err != nil {
		return Page{}, err
	}
	var issues []struct {
		Number    int    `json:"number"`
		Title     string `json:"title"`
		Body      string `json:"body"`
		State     string `json:"state"`
		HTMLURL   string `json:"html_url"`
		UpdatedAt string `json:"updated_at"`
		Comments  int    `json:"comments"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct{} `json:"pull_request"`
	}
	if err := getJSON(ctx, in.Client, req, &issues); err != nil {
		return Page{}, err
	}

	page := Page{Cursor: in.Cursor, More: len(issues) >= limit}
	for _, is := range issues {
		kind := "issue"
		if is.PullRequest != nil {
			kind = "pull request"
		}
		var b strings.Builder
		fmt.Fprintf(&b, "**%s** · %s · opened by %s\n\n", kind, is.State, is.User.Login)
		b.WriteString(strings.TrimSpace(is.Body))
		b.WriteString("\n\n")
		if is.Comments > 0 {
			comments, err := g.comments(ctx, in, api, repo, is.Number, headers)
			if err == nil && len(comments) > 0 {
				b.WriteString("## Comments\n\n")
				b.WriteString(strings.Join(comments, "\n\n"))
			}
		}
		labels := make([]string, len(is.Labels))
		for i, l := range is.Labels {
			labels[i] = l.Name
		}
		page.Docs = append(page.Docs, Document{
			ExternalID: fmt.Sprintf("%s#%d", repo, is.Number),
			Title:      fmt.Sprintf("#%d %s", is.Number, is.Title),
			Body:       b.String(),
			URL:        is.HTMLURL,
			Updated:    is.UpdatedAt,
			Author:     is.User.Login,
			Meta: map[string]string{"repo": repo, "kind": kind, "state": is.State,
				"labels": strings.Join(labels, ", "), "source": "github"},
		})
		if is.UpdatedAt > page.Cursor {
			page.Cursor = is.UpdatedAt
		}
	}

	if strings.EqualFold(in.Config.Get("docs"), "yes") && in.Cursor == "" {
		// Repository markdown is pulled on the FIRST sync only: the issues API
		// has a since parameter and the contents API does not, so re-walking
		// the tree every few minutes would spend the whole rate limit
		// rediscovering files that have not changed.
		docs, err := g.markdown(ctx, in, api, repo, headers)
		if err == nil {
			page.Docs = append(page.Docs, docs...)
		}
	}
	if page.Cursor == "" {
		page.Cursor = rfc3339(time.Now())
	}
	return page, nil
}

func (g github) comments(ctx context.Context, in Input, api, repo string, number int,
	headers map[string]string) ([]string, error) {
	req, err := jsonRequest(fmt.Sprintf("%s/repos/%s/issues/%d/comments", api, repo, number),
		url.Values{"per_page": {"100"}}, headers)
	if err != nil {
		return nil, err
	}
	var out []struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(out))
	for _, c := range out {
		lines = append(lines, fmt.Sprintf("**%s** · %s\n\n%s",
			c.User.Login, c.CreatedAt, strings.TrimSpace(c.Body)))
	}
	return lines, nil
}

// markdown pulls every *.md file from the default branch.
func (g github) markdown(ctx context.Context, in Input, api, repo string,
	headers map[string]string) ([]Document, error) {
	var repoInfo struct {
		DefaultBranch string `json:"default_branch"`
	}
	req, err := jsonRequest(api+"/repos/"+repo, nil, headers)
	if err != nil {
		return nil, err
	}
	if err := getJSON(ctx, in.Client, req, &repoInfo); err != nil {
		return nil, err
	}
	req, err = jsonRequest(fmt.Sprintf("%s/repos/%s/git/trees/%s", api, repo, repoInfo.DefaultBranch),
		url.Values{"recursive": {"1"}}, headers)
	if err != nil {
		return nil, err
	}
	var tree struct {
		Tree []struct {
			Path string `json:"path"`
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tree"`
	}
	if err := getJSON(ctx, in.Client, req, &tree); err != nil {
		return nil, err
	}

	var docs []Document
	for _, entry := range tree.Tree {
		if entry.Type != "blob" || !strings.HasSuffix(strings.ToLower(entry.Path), ".md") {
			continue
		}
		if entry.Size > 1<<20 {
			continue // a megabyte of markdown is a generated file, not a document
		}
		req, err := jsonRequest(fmt.Sprintf("%s/repos/%s/contents/%s", api, repo, entry.Path),
			url.Values{"ref": {repoInfo.DefaultBranch}}, headers)
		if err != nil {
			continue
		}
		var file struct {
			Content  string `json:"content"`
			Encoding string `json:"encoding"`
			HTMLURL  string `json:"html_url"`
		}
		if err := getJSON(ctx, in.Client, req, &file); err != nil {
			continue
		}
		body := file.Content
		if file.Encoding == "base64" {
			raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
			if err != nil {
				continue
			}
			body = string(raw)
		}
		docs = append(docs, Document{
			ExternalID: repo + ":" + entry.Path,
			Title:      entry.Path,
			Body:       body,
			URL:        file.HTMLURL,
			Updated:    rfc3339(time.Now()),
			Meta:       map[string]string{"repo": repo, "path": entry.Path, "source": "github"},
		})
	}
	return docs, nil
}
