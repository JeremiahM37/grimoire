package render

import (
	"strings"
	"testing"
	"time"
)

// A live template renders where it sits, every time the page is read — which
// is the difference between a widget and a snippet, and most of what these
// tests are about.

func templateCtx(t *testing.T, notes map[string]string) *Context {
	t.Helper()
	linkMap := map[string]string{}
	for path := range notes {
		linkMap[strings.ToLower(path)] = path
		linkMap[strings.ToLower(stripMD(path))] = path
	}
	return &Context{
		LinkMap: linkMap,
		NoteBody: func(rel string) *string {
			if body, ok := notes[rel]; ok {
				return &body
			}
			return nil
		},
		TemplateBody: func(name string) *string {
			if body, ok := notes[TemplatePrefix+name+".md"]; ok {
				return &body
			}
			return nil
		},
	}
}

func pin(t *testing.T, when time.Time) {
	t.Helper()
	old := Now
	Now = func() time.Time { return when }
	t.Cleanup(func() { Now = old })
}

func TestTemplateBlockRendersTheTemplate(t *testing.T) {
	ctx := templateCtx(t, map[string]string{
		"templates/standup.md": "## Standup for {{owner}}\n\n- what shipped\n",
	})
	got := RenderWith("```template\nuse: standup\nowner: Ana\n```\n", ctx)
	if !strings.Contains(got, "Standup for Ana") {
		t.Errorf("variable not substituted:\n%s", got)
	}
	// Rendered as markdown, not dumped as text — that is what makes it a page
	// part rather than a snippet.
	if !strings.Contains(got, "<h2") || !strings.Contains(got, "<li>") {
		t.Errorf("template body was not rendered:\n%s", got)
	}
	if !strings.Contains(got, `data-template="standup"`) {
		t.Errorf("rendered block does not say which template it is:\n%s", got)
	}
}

func TestTemplateBlockRunsQueriesInside(t *testing.T) {
	// The whole point: a "weekly review" template that lists open tasks has to
	// be right on Wednesday too.
	ctx := templateCtx(t, map[string]string{
		"templates/review.md": "## Review\n\n```query\nfrom: tasks\nchecked: false\n```\n",
	})
	ran := ""
	ctx.RunQuery = func(block string) *QueryResult {
		ran = block
		return &QueryResult{Render: "count", Rows: []map[string]any{{"path": "a.md"}}}
	}
	got := RenderWith("```template\nuse: review\n```\n", ctx)
	if !strings.Contains(ran, "from: tasks") {
		t.Errorf("the query inside the template did not run (ran %q)", ran)
	}
	if !strings.Contains(got, "query-count") {
		t.Errorf("query output missing:\n%s", got)
	}
}

func TestTemplateBuiltInDates(t *testing.T) {
	pin(t, time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC))
	ctx := templateCtx(t, map[string]string{
		"templates/daily.md": "{{date}} / {{weekday}} / {{yesterday}} / {{tomorrow}}\n",
	})
	got := RenderWith("```template\nuse: daily\n```\n", ctx)
	for _, want := range []string{"2026-08-20", "Thursday", "2026-08-19", "2026-08-21"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q:\n%s", want, got)
		}
	}
}

func TestGivenVariablesBeatBuiltIns(t *testing.T) {
	pin(t, time.Date(2026, 8, 20, 9, 30, 0, 0, time.UTC))
	ctx := templateCtx(t, map[string]string{"templates/d.md": "{{date}}\n"})
	got := RenderWith("```template\nuse: d\ndate: 1999-12-31\n```\n", ctx)
	if !strings.Contains(got, "1999-12-31") {
		t.Errorf("an explicit value lost to the built-in:\n%s", got)
	}
}

func TestUnfilledPlaceholderIsLeftVisible(t *testing.T) {
	// Blanking it produces a page that reads as finished and is missing
	// something, which is the failure nobody notices.
	ctx := templateCtx(t, map[string]string{"templates/t.md": "owner: {{owner}}\n"})
	got := RenderWith("```template\nuse: t\n```\n", ctx)
	if !strings.Contains(got, "{{owner}}") {
		t.Errorf("an unfilled slot was silently blanked:\n%s", got)
	}
}

func TestTemplateBlockErrors(t *testing.T) {
	ctx := templateCtx(t, map[string]string{"templates/t.md": "body\n"})
	cases := map[string]string{
		"```template\nowner: Ana\n```\n": "needs 'use: name'",
		"```template\nuse: nope\n```\n":  "not found",
	}
	for block, want := range cases {
		got := RenderWith(block, ctx)
		if !strings.Contains(got, want) {
			t.Errorf("%q rendered %s, want %q", block, got, want)
		}
	}
}

func TestTemplateBlockCannotPullAnArbitraryNote(t *testing.T) {
	// Only the templates directory: otherwise a template block is a way to
	// embed any note under a friendlier name, and unlike ![[embed]] it does
	// not say which note it pulled in.
	ctx := templateCtx(t, map[string]string{
		"secrets.md":     "the severance terms\n",
		"templates/t.md": "a real template\n",
	})
	got := RenderWith("```template\nuse: secrets\n```\n", ctx)
	if strings.Contains(got, "severance") {
		t.Errorf("a template block embedded an ordinary note:\n%s", got)
	}
}

func TestTemplateRecursionIsBounded(t *testing.T) {
	// A template that uses itself must be caught rather than looping.
	ctx := templateCtx(t, map[string]string{
		"templates/loop.md": "before\n\n```template\nuse: loop\n```\n\nafter\n",
	})
	done := make(chan string, 1)
	go func() { done <- RenderWith("```template\nuse: loop\n```\n", ctx) }()
	select {
	case got := <-done:
		if !strings.Contains(got, "depth limit") {
			t.Errorf("recursion was not reported:\n%s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("rendering a self-using template did not terminate")
	}
}

func TestTemplateWithoutASourceSaysSo(t *testing.T) {
	got := RenderWith("```template\nuse: t\n```\n", &Context{})
	if !strings.Contains(got, "not available here") {
		t.Errorf("got %s", got)
	}
}

func TestOtherFencesAreStillCode(t *testing.T) {
	ctx := templateCtx(t, map[string]string{"templates/t.md": "body\n"})
	got := RenderWith("```bash\nuse: t\n```\n", ctx)
	if !strings.Contains(got, "<pre><code") {
		t.Errorf("a bash fence stopped being code:\n%s", got)
	}
}

func TestTemplateVarsListsWhatMustBeFilled(t *testing.T) {
	got := TemplateVars("{{owner}} on {{date}} about {{topic}} — {{owner}} again\n")
	if len(got) != 2 || got[0] != "owner" || got[1] != "topic" {
		t.Errorf("TemplateVars = %v, want [owner topic]", got)
	}
}

func TestExpandVarsIsCaseInsensitiveOnNames(t *testing.T) {
	out := ExpandVars("{{Owner}}", map[string]string{"owner": "Ana"}, time.Time{})
	if out != "Ana" {
		t.Errorf("got %q", out)
	}
}
