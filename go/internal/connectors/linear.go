package connectors

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Linear: issues, their descriptions and their comments.
//
// The same knowledge Jira holds, for the teams that left Jira. Linear's API is
// GraphQL only, so unlike the REST sources this one sends a query rather than
// building a URL — which is also why its pagination is a cursor from the
// response rather than a timestamp we invent.

func init() { Register(linear{}) }

type linear struct{}

func (linear) Kind() string { return "linear" }

func (linear) Describe() Kind {
	return Kind{
		Kind: "linear",
		Name: "Linear",
		Help: "Pulls issues with their descriptions and comments. Leave the team key " +
			"empty to pull every team you can see.",
		SecretHelp: "A personal API key from Linear → Settings → Security & access → " +
			"Personal API keys. Read access is enough.",
		Fields: []Field{
			{Name: "team", Label: "Team key", Placeholder: "ENG",
				Help: "Optional. The short prefix on issue ids, e.g. ENG in ENG-123."},
		},
		DefaultPrefix: "connectors/linear",
	}
}

// linearQuery asks for a page of issues ordered by updatedAt, so the cursor is
// a genuine resume point rather than a re-scan.
const linearQuery = `query($first:Int!,$after:String,$filter:IssueFilter){
  issues(first:$first, after:$after, filter:$filter, orderBy:updatedAt){
    pageInfo{ hasNextPage endCursor }
    nodes{
      id identifier title description url updatedAt
      state{ name }
      team{ key name }
      assignee{ name }
      labels{ nodes{ name } }
      comments{ nodes{ body createdAt user{ name } } }
    }
  }
}`

type linearResp struct {
	Data struct {
		Issues struct {
			PageInfo struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []struct {
				ID          string                     `json:"id"`
				Identifier  string                     `json:"identifier"`
				Title       string                     `json:"title"`
				Description string                     `json:"description"`
				URL         string                     `json:"url"`
				UpdatedAt   string                     `json:"updatedAt"`
				State       struct{ Name string }      `json:"state"`
				Team        struct{ Key, Name string } `json:"team"`
				Assignee    struct{ Name string }      `json:"assignee"`
				Labels      struct {
					Nodes []struct{ Name string } `json:"nodes"`
				} `json:"labels"`
				Comments struct {
					Nodes []struct {
						Body      string                `json:"body"`
						CreatedAt string                `json:"createdAt"`
						User      struct{ Name string } `json:"user"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"nodes"`
		} `json:"issues"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

func (l linear) Fetch(ctx context.Context, in Input) (Page, error) {
	if in.Secret == "" {
		return Page{}, fmt.Errorf("%w: an API key is required", ErrConfig)
	}
	limit := in.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	vars := map[string]any{"first": limit}
	if in.Cursor != "" {
		vars["after"] = in.Cursor
	}
	if team := in.Config.Get("team"); team != "" {
		vars["filter"] = map[string]any{"team": map[string]any{"key": map[string]any{"eq": team}}}
	}
	raw, err := json.Marshal(map[string]any{"query": linearQuery, "variables": vars})
	if err != nil {
		return Page{}, err
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.linear.app/graphql", bytes.NewReader(raw))
	if err != nil {
		return Page{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", in.Secret)

	var out linearResp
	if err := getJSON(ctx, in.Client, req, &out); err != nil {
		return Page{}, err
	}
	// GraphQL reports failures in a 200 body, so an unchecked error here looks
	// like an empty sync forever rather than a broken credential.
	if len(out.Errors) > 0 {
		return Page{}, fmt.Errorf("linear: %s", out.Errors[0].Message)
	}

	nodes := out.Data.Issues.Nodes
	docs := make([]Document, 0, len(nodes))
	for _, n := range nodes {
		var b strings.Builder
		if n.State.Name != "" {
			fmt.Fprintf(&b, "**Status:** %s", n.State.Name)
			if n.Assignee.Name != "" {
				fmt.Fprintf(&b, " · **Assignee:** %s", n.Assignee.Name)
			}
			b.WriteString("\n\n")
		}
		if d := strings.TrimSpace(n.Description); d != "" {
			b.WriteString(d)
			b.WriteString("\n\n")
		}
		if len(n.Comments.Nodes) > 0 {
			b.WriteString("## Comments\n\n")
			for _, c := range n.Comments.Nodes {
				who := c.User.Name
				if who == "" {
					who = "someone"
				}
				fmt.Fprintf(&b, "**%s** (%s)\n\n%s\n\n", who, c.CreatedAt, strings.TrimSpace(c.Body))
			}
		}
		labels := make([]string, 0, len(n.Labels.Nodes))
		for _, lb := range n.Labels.Nodes {
			labels = append(labels, lb.Name)
		}
		meta := map[string]string{"status": n.State.Name, "team": n.Team.Key}
		if len(labels) > 0 {
			meta["labels"] = strings.Join(labels, ", ")
		}
		if n.Assignee.Name != "" {
			meta["assignee"] = n.Assignee.Name
		}
		title := n.Title
		if n.Identifier != "" {
			title = n.Identifier + " " + n.Title
		}
		docs = append(docs, Document{
			ExternalID: n.ID,
			Title:      title,
			Body:       strings.TrimSpace(b.String()),
			URL:        n.URL,
			Updated:    n.UpdatedAt,
			Author:     n.Assignee.Name,
			Meta:       meta,
		})
	}
	return Page{
		Docs:   docs,
		Cursor: out.Data.Issues.PageInfo.EndCursor,
		More:   out.Data.Issues.PageInfo.HasNextPage,
	}, nil
}
