package connectors

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Confluence and Jira. Both are Atlassian Cloud REST APIs with the same auth
// (email + API token, HTTP basic) and the same incremental trick: a query
// language with a "changed since" clause, so a sync asks for what moved rather
// than paging the whole instance.
//
// Both are also available self-hosted, where the site URL is on a private
// network — which the outbound guard blocks by default. That is why the runner
// can be told to allow private addresses; without it, a self-hosted Confluence
// fails with an error about link-local addresses that reads like a bug.

func init() { Register(confluence{}); Register(jira{}) }

// basicAuth builds the Authorization header both use.
func basicAuth(email, token string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
}

// ------------------------------------------------------------- confluence

type confluence struct{}

func (confluence) Kind() string { return "confluence" }

func (confluence) Describe() Kind {
	return Kind{
		Kind: "confluence",
		Name: "Confluence",
		Help: "Pulls pages from one or more spaces, newest changes first. Works " +
			"with Atlassian Cloud and with self-hosted Data Center.",
		SecretHelp: "An API token from id.atlassian.com/manage-profile/security/api-tokens. " +
			"Store it as the secret; put the account email in the field below.",
		Fields: []Field{
			{Name: "site", Label: "Site URL", Required: true,
				Placeholder: "https://yourteam.atlassian.net"},
			{Name: "email", Label: "Account email", Required: true,
				Placeholder: "you@example.com",
				Help:        "The account the API token belongs to."},
			{Name: "spaces", Label: "Space keys", Placeholder: "ENG, HANDBOOK",
				Help: "Comma-separated. Leave empty for every space the account can read."},
		},
		DefaultPrefix: "connectors/confluence",
	}
}

func (c confluence) Fetch(ctx context.Context, in Input) (Page, error) {
	site, err := baseURL(in.Config.Get("site"))
	if err != nil {
		return Page{}, err
	}
	email := in.Config.Get("email")
	if email == "" || in.Secret == "" {
		return Page{}, missing("an account email and an API token")
	}

	cql := "type=page"
	if spaces := splitList(in.Config.Get("spaces")); len(spaces) > 0 {
		quoted := make([]string, len(spaces))
		for i, s := range spaces {
			quoted[i] = `"` + s + `"`
		}
		cql += " and space in (" + strings.Join(quoted, ",") + ")"
	}
	if in.Cursor != "" {
		// Confluence's CQL takes a date, not a timestamp, and compares in the
		// instance's timezone. Asking from the cursor's DAY re-fetches a few
		// already-seen pages rather than missing ones written later the same
		// day — the index deduplicates by external id, so the cost is a couple
		// of redundant updates and the alternative is silent data loss.
		if day, _, ok := strings.Cut(in.Cursor, "T"); ok {
			cql += fmt.Sprintf(" and lastModified >= %q", day)
		}
	}
	cql += " order by lastModified asc"

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{
		"cql":    {cql},
		"limit":  {strconv.Itoa(limit)},
		"expand": {"body.storage,version,space"},
	}
	req, err := jsonRequest(site+"/wiki/rest/api/content/search", q,
		map[string]string{"Authorization": basicAuth(email, in.Secret)})
	if err != nil {
		return Page{}, err
	}
	var out struct {
		Results []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Space struct {
				Key string `json:"key"`
			} `json:"space"`
			Version struct {
				When string `json:"when"`
				By   struct {
					DisplayName string `json:"displayName"`
				} `json:"by"`
			} `json:"version"`
			Body struct {
				Storage struct {
					Value string `json:"value"`
				} `json:"storage"`
			} `json:"body"`
			Links struct {
				WebUI string `json:"webui"`
			} `json:"_links"`
		} `json:"results"`
		Size  int `json:"size"`
		Limit int `json:"limit"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}

	page := Page{Cursor: in.Cursor, More: out.Size >= out.Limit && out.Limit > 0}
	for _, r := range out.Results {
		body := HTMLToMarkdown(r.Body.Storage.Value)
		page.Docs = append(page.Docs, Document{
			ExternalID: r.ID,
			Title:      r.Title,
			Body:       body,
			URL:        site + "/wiki" + r.Links.WebUI,
			Updated:    r.Version.When,
			Author:     r.Version.By.DisplayName,
			Meta:       map[string]string{"space": r.Space.Key, "source": "confluence"},
		})
		if r.Version.When > page.Cursor {
			page.Cursor = r.Version.When
		}
	}
	return page, nil
}

// ------------------------------------------------------------------- jira

type jira struct{}

func (jira) Kind() string { return "jira" }

func (jira) Describe() Kind {
	return Kind{
		Kind: "jira",
		Name: "Jira",
		Help: "Pulls issues from the projects you list, including their description " +
			"and comments, so a ticket reads as one document.",
		SecretHelp: "An API token from id.atlassian.com/manage-profile/security/api-tokens, " +
			"with the account email in the field below.",
		Fields: []Field{
			{Name: "site", Label: "Site URL", Required: true,
				Placeholder: "https://yourteam.atlassian.net"},
			{Name: "email", Label: "Account email", Required: true, Placeholder: "you@example.com"},
			{Name: "projects", Label: "Project keys", Placeholder: "ENG, OPS",
				Help: "Comma-separated. Leave empty for every project the account can see."},
			{Name: "jql", Label: "Extra JQL", Placeholder: "status != Done",
				Help: "Optional. ANDed with the project and updated-since clauses."},
		},
		DefaultPrefix: "connectors/jira",
	}
}

func (j jira) Fetch(ctx context.Context, in Input) (Page, error) {
	site, err := baseURL(in.Config.Get("site"))
	if err != nil {
		return Page{}, err
	}
	email := in.Config.Get("email")
	if email == "" || in.Secret == "" {
		return Page{}, missing("an account email and an API token")
	}

	var clauses []string
	if projects := splitList(in.Config.Get("projects")); len(projects) > 0 {
		clauses = append(clauses, "project in ("+strings.Join(projects, ",")+")")
	}
	if extra := in.Config.Get("jql"); extra != "" {
		clauses = append(clauses, "("+extra+")")
	}
	if in.Cursor != "" {
		if t, err := time.Parse(time.RFC3339, in.Cursor); err == nil {
			// JQL compares in minutes and in the instance's timezone; the same
			// reasoning as Confluence applies, so the window is nudged back.
			clauses = append(clauses, fmt.Sprintf("updated >= %q",
				t.Add(-2*time.Minute).UTC().Format("2006-01-02 15:04")))
		}
	}
	jql := strings.Join(clauses, " AND ")
	if jql == "" {
		jql = "order by updated asc"
	} else {
		jql += " order by updated asc"
	}

	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{
		"jql":        {jql},
		"maxResults": {strconv.Itoa(limit)},
		"fields":     {"summary,description,updated,status,assignee,reporter,labels,comment,project"},
	}
	req, err := jsonRequest(site+"/rest/api/3/search", q,
		map[string]string{"Authorization": basicAuth(email, in.Secret)})
	if err != nil {
		return Page{}, err
	}
	var out struct {
		Total      int `json:"total"`
		MaxResults int `json:"maxResults"`
		Issues     []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary     string                       `json:"summary"`
				Description json.RawMessage              `json:"description"`
				Updated     string                       `json:"updated"`
				Labels      []string                     `json:"labels"`
				Status      struct{ Name string }        `json:"status"`
				Assignee    struct{ DisplayName string } `json:"assignee"`
				Reporter    struct{ DisplayName string } `json:"reporter"`
				Project     struct{ Key string }         `json:"project"`
				Comment     struct {
					Comments []struct {
						Author  struct{ DisplayName string } `json:"author"`
						Created string                       `json:"created"`
						Body    json.RawMessage              `json:"body"`
					} `json:"comments"`
				} `json:"comment"`
			} `json:"fields"`
		} `json:"issues"`
	}
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}

	page := Page{Cursor: in.Cursor, More: len(out.Issues) >= limit}
	for _, is := range out.Issues {
		f := is.Fields
		var b strings.Builder
		fmt.Fprintf(&b, "**%s** · %s", f.Status.Name, f.Project.Key)
		if f.Assignee.DisplayName != "" {
			fmt.Fprintf(&b, " · assigned to %s", f.Assignee.DisplayName)
		}
		b.WriteString("\n\n")
		if d := ADFToMarkdown(f.Description); d != "" {
			b.WriteString(d + "\n\n")
		}
		if len(f.Comment.Comments) > 0 {
			b.WriteString("## Comments\n\n")
			for _, c := range f.Comment.Comments {
				fmt.Fprintf(&b, "**%s** · %s\n\n%s\n\n",
					c.Author.DisplayName, c.Created, ADFToMarkdown(c.Body))
			}
		}
		updated := normalizeJiraTime(f.Updated)
		page.Docs = append(page.Docs, Document{
			ExternalID: is.Key,
			Title:      is.Key + " " + f.Summary,
			Body:       b.String(),
			URL:        site + "/browse/" + is.Key,
			Updated:    updated,
			Author:     f.Reporter.DisplayName,
			Meta: map[string]string{"project": f.Project.Key, "status": f.Status.Name,
				"labels": strings.Join(f.Labels, ", "), "source": "jira"},
		})
		if updated > page.Cursor {
			page.Cursor = updated
		}
	}
	return page, nil
}

// normalizeJiraTime converts Jira's "2026-08-19T10:00:00.000+0000" to RFC3339,
// so cursors from different sources compare the same way.
func normalizeJiraTime(s string) string {
	for _, layout := range []string{"2006-01-02T15:04:05.000-0700", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return rfc3339(t)
		}
	}
	return s
}
