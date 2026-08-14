package render

import (
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/compat"
)

type renderFixture struct {
	Cases []struct {
		Name     string            `json:"name"`
		Markdown string            `json:"markdown"`
		LinkMap  map[string]string `json:"link_map"`
		HTML     string            `json:"html"`
	} `json:"cases"`
	HeadingIDCases []struct {
		Text string `json:"text"`
		ID   string `json:"id"`
	} `json:"heading_id_cases"`
}

func load(t *testing.T) renderFixture {
	t.Helper()
	var fx renderFixture
	compat.Load(t, "render.json", &fx)
	return fx
}

// The renderer's HTML is a byte contract — the client renderer and the
// vision-checked /read pages both depend on this exact output.
func TestRenderMatchesPython(t *testing.T) {
	fx := load(t)
	if len(fx.Cases) == 0 {
		t.Fatal("no render cases in fixture")
	}
	for _, c := range fx.Cases {
		got := Render(c.Markdown, c.LinkMap)
		if got != c.HTML {
			t.Errorf("case %q diverged:\n got: %s\nwant: %s", c.Name, got, c.HTML)
		}
	}
}

func TestHeadingIDMatchesPython(t *testing.T) {
	for _, c := range load(t).HeadingIDCases {
		if got := HeadingID(c.Text); got != c.ID {
			t.Errorf("HeadingID(%q) = %q, want %q", c.Text, got, c.ID)
		}
	}
}

// Python's html.escape emits &quot; and &#x27; where Go's html.EscapeString
// emits &#34; and &#39; — same meaning, different bytes, and the fixture
// compares bytes.
func TestEscapeMatchesPythonEntities(t *testing.T) {
	got := escapeHTML(`<a href="x">&'</a>`)
	want := `&lt;a href=&quot;x&quot;&gt;&amp;&#x27;&lt;/a&gt;`
	if got != want {
		t.Errorf("escapeHTML:\n got: %s\nwant: %s", got, want)
	}
}

// The emphasis rule needs lookarounds RE2 lacks; check the boundaries the
// hand-rolled scanner exists to get right.
func TestEmphasisBoundaries(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"*italic*", "<em>italic</em>"},
		{"a *b* c", "a <em>b</em> c"},
		{"**not italic**", "**not italic**"}, // ** is consumed by strong first
		{"***triple***", "***triple***"},     // guarded by both lookarounds
		{"a * b", "a * b"},                   // lone star
		{"*a* and *b*", "<em>a</em> and <em>b</em>"},
		{"", ""},
	} {
		if got := replaceEmphasis(tc.in); got != tc.want {
			t.Errorf("replaceEmphasis(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Transclusion and query blocks aren't exercised by the fixture (it renders
// with a bare context), so cover the guards directly — an unbounded embed loop
// would hang the server.
func TestTransclusionIsDepthLimitedAndCycleSafe(t *testing.T) {
	bodies := map[string]string{
		"a.md": "# A\n\n![[b]]\n",
		"b.md": "# B\n\n![[a]]\n",
	}
	ctx := &Context{
		LinkMap: map[string]string{"a": "a.md", "b": "b.md"},
		NoteBody: func(rel string) *string {
			if s, ok := bodies[rel]; ok {
				return &s
			}
			return nil
		},
	}
	out := RenderWith(bodies["a.md"], ctx)
	if !strings.Contains(out, "embed-cycle") {
		t.Errorf("cycle was not cut off:\n%s", out)
	}
	if strings.Count(out, "embed-title") > MaxEmbedDepth {
		t.Errorf("embedded deeper than MaxEmbedDepth:\n%s", out)
	}
}

func TestMissingTransclusionTargetIsReported(t *testing.T) {
	ctx := &Context{
		LinkMap:  map[string]string{},
		NoteBody: func(string) *string { return nil },
	}
	out := RenderWith("![[nowhere]]\n", ctx)
	if !strings.Contains(out, "embed-missing") {
		t.Errorf("missing target not reported:\n%s", out)
	}
}
