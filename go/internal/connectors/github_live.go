package connectors

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Live search, read and write for GitHub issues. The sync side is github.go.
// Everything is pinned to the connector's configured repository: an agent that
// can search or write "anywhere the token reaches" has been handed far more
// than the operator set this connector up for.

func (g github) api(in Input) (string, map[string]string) {
	api := strings.TrimRight(in.Config.Get("api"), "/")
	if api == "" {
		api = "https://api.github.com"
	}
	h := map[string]string{"Accept": "application/vnd.github+json"}
	if in.Secret != "" {
		h["Authorization"] = "Bearer " + in.Secret
	}
	return api, h
}

func (g github) Search(ctx context.Context, in Input, query string, limit int) ([]Hit, error) {
	if limit <= 0 || limit > 25 {
		limit = 10
	}
	api, h := g.api(in)
	// Scope to the configured repo whatever the query says: strip any repo:
	// qualifier the caller supplied and add ours.
	var terms []string
	for _, t := range strings.Fields(query) {
		if !strings.HasPrefix(strings.ToLower(t), "repo:") {
			terms = append(terms, t)
		}
	}
	q := strings.Join(terms, " ") + " repo:" + in.Config.Get("repo")
	req, err := jsonRequest(api+"/search/issues", url.Values{"q": {q}, "per_page": {strconv.Itoa(limit)}}, h)
	if err != nil {
		return nil, err
	}
	var out struct {
		Items []struct {
			Number    int    `json:"number"`
			Title     string `json:"title"`
			Body      string `json:"body"`
			State     string `json:"state"`
			HTMLURL   string `json:"html_url"`
			UpdatedAt string `json:"updated_at"`
			User      struct {
				Login string `json:"login"`
			} `json:"user"`
		} `json:"items"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return nil, err
	}
	var hits []Hit
	for _, it := range out.Items {
		snip := strings.TrimSpace(it.Body)
		if len(snip) > 300 {
			snip = snip[:300] + "…"
		}
		hits = append(hits, Hit{ID: fmt.Sprintf("%s#%d", in.Config.Get("repo"), it.Number),
			Title: fmt.Sprintf("#%d %s [%s]", it.Number, it.Title, it.State), Snippet: snip,
			URL: it.HTMLURL, Updated: it.UpdatedAt, Author: it.User.Login})
	}
	return hits, nil
}

func (g github) Read(ctx context.Context, in Input, id string) (Item, error) {
	repo, num, ok := strings.Cut(id, "#")
	n, err := strconv.Atoi(num)
	if !ok || err != nil || repo != in.Config.Get("repo") {
		return Item{}, fmt.Errorf("github ids look like %s#NUMBER", in.Config.Get("repo"))
	}
	api, h := g.api(in)
	req, err := jsonRequest(fmt.Sprintf("%s/repos/%s/issues/%d", api, repo, n), nil, h)
	if err != nil {
		return Item{}, err
	}
	var is struct {
		Title, Body, State, HTMLURL string
		UpdatedAt                   string `json:"updated_at"`
		User                        struct{ Login string }
	}
	if err := getJSON(ctx, in.Client, req, &is); err != nil {
		return Item{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** · opened by %s\n\n%s\n\n", is.State, is.User.Login, strings.TrimSpace(is.Body))
	if cs, err := g.comments(ctx, in, api, repo, n, h); err == nil && len(cs) > 0 {
		b.WriteString("## Comments\n\n" + strings.Join(cs, "\n\n"))
	}
	return Item{ID: id, Title: fmt.Sprintf("#%d %s", n, is.Title), Body: strings.TrimSpace(b.String()),
		URL: is.HTMLURL, Updated: is.UpdatedAt, Author: is.User.Login}, nil
}

func (github) Actions() []ActionSpec {
	return []ActionSpec{
		{Name: "create_issue", Summary: "Open an issue in the connector's repository.",
			Params: []Field{{Name: "title", Label: "Title", Required: true}, {Name: "body", Label: "Body"},
				{Name: "labels", Label: "Labels, comma-separated"}},
			Scopes: []string{"issues:write (fine-grained) or repo"}, Irreversible: true},
		{Name: "comment", Summary: "Comment on an issue or pull request in the connector's repository.",
			Params: []Field{{Name: "number", Label: "Issue or PR number", Required: true},
				{Name: "body", Label: "Comment", Required: true}},
			Scopes: []string{"issues:write (fine-grained) or repo"}, Irreversible: true},
	}
}

func (g github) Act(ctx context.Context, in Input, action string, p map[string]string) (ActionResult, error) {
	repo := in.Config.Get("repo")
	if r := p["repo"]; r != "" && r != repo {
		return ActionResult{}, fmt.Errorf("this connector only acts on %s", repo)
	}
	api, h := g.api(in)
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	switch action {
	case "create_issue":
		body := map[string]any{"title": p["title"], "body": p["body"]}
		if l := splitList(p["labels"]); len(l) > 0 {
			body["labels"] = l
		}
		if err := sendJSON(ctx, in, "POST", fmt.Sprintf("%s/repos/%s/issues", api, repo), body, &out, h); err != nil {
			return ActionResult{}, err
		}
		return ActionResult{ID: fmt.Sprintf("%s#%d", repo, out.Number), URL: out.HTMLURL, Message: "issue created"}, nil
	case "comment":
		n, err := strconv.Atoi(strings.TrimSpace(p["number"]))
		if err != nil || n <= 0 {
			return ActionResult{}, fmt.Errorf("number must be an issue number")
		}
		if err := sendJSON(ctx, in, "POST", fmt.Sprintf("%s/repos/%s/issues/%d/comments", api, repo, n),
			map[string]any{"body": p["body"]}, &out, h); err != nil {
			return ActionResult{}, err
		}
		return ActionResult{ID: fmt.Sprintf("%s#%d", repo, n), URL: out.HTMLURL, Message: "comment posted"}, nil
	}
	return ActionResult{}, fmt.Errorf("github has no action %q", action)
}
