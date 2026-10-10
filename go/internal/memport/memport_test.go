package memport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestDetectByShape(t *testing.T) {
	cases := map[string]string{
		"mem0.json":         SourceMem0,
		"letta_agent.json":  SourceLetta,
		"letta_blocks.json": SourceLetta,
		"zep.json":          SourceZep,
		"generic.jsonl":     SourceGeneric,
		"grimoire.jsonl":    SourceGrimoire,
	}
	for name, want := range cases {
		got, err := Detect(fixture(t, name))
		if err != nil || got != want {
			t.Errorf("%s: Detect = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestDetectRefusesUnknownShapes(t *testing.T) {
	for _, in := range []string{"", `{"hello": 1}`, `[{"other": true}]`, "not json at all"} {
		if _, err := Detect([]byte(in)); err == nil {
			t.Errorf("Detect(%q) accepted an unknown shape", in)
		}
	}
}

func TestParseMem0(t *testing.T) {
	res, err := Parse(fixture(t, "mem0.json"), "auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || len(res.Skips) != 1 {
		t.Fatalf("items=%d skips=%d, want 2 and 1", len(res.Items), len(res.Skips))
	}
	first := res.Items[0]
	if first.Restored {
		t.Error("a foreign record must not be restored")
	}
	if first.Record.Origin != "import:mem0" || first.Record.Agent != "import:mem0" {
		t.Errorf("origin/agent = %q/%q", first.Record.Origin, first.Record.Agent)
	}
	if first.Record.Category != "preference" || first.Record.Stamp != "2026-03-02 10:15" {
		// The stamp is the store's display form, in local time.
		want := mustLocalStamp(t, "2026-03-02T10:15:00Z")
		if first.Record.Stamp != want || first.Record.Category != "preference" {
			t.Errorf("record = %+v", first.Record)
		}
	}
	if res.Items[1].Record.Session != "run-42" {
		t.Errorf("run_id should map to session, got %q", res.Items[1].Record.Session)
	}
}

func TestParseLettaAgentFile(t *testing.T) {
	res, err := Parse(fixture(t, "letta_agent.json"), "letta")
	if err != nil {
		t.Fatal(err)
	}
	// Two non-empty blocks and one archival passage; the empty block is skipped.
	if len(res.Items) != 3 || len(res.Skips) != 1 {
		t.Fatalf("items=%d skips=%d: %+v", len(res.Items), len(res.Skips), res)
	}
	byText := map[string]Record{}
	for _, it := range res.Items {
		byText[it.Record.Text] = it.Record
	}
	if got := byText["Name is Jeremiah. Works nights."]; got.Category != "human" || got.Task != "letta agent homelab" {
		t.Errorf("block record = %+v", got)
	}
	if got := byText["Restic repo lives on MediaServer."]; got.Category != "archival" {
		t.Errorf("passage record = %+v", got)
	}
}

func TestParseLettaBareBlocks(t *testing.T) {
	res, err := Parse(fixture(t, "letta_blocks.json"), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Record.Category != "persona" {
		t.Fatalf("got %+v", res)
	}
}

func TestParseZepKeepsValidityAsProvenance(t *testing.T) {
	res, err := Parse(fixture(t, "zep.json"), "zep")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %d", len(res.Items))
	}
	if strings.Contains(res.Items[0].Record.Text, "valid") {
		t.Error("validity must stay out of the fact text")
	}
	if !strings.Contains(res.Items[1].Record.Task, "invalid_at") {
		t.Errorf("invalid_at should be recorded as provenance, got %q", res.Items[1].Record.Task)
	}
	if res.Items[0].Record.Expires != "" {
		t.Error("zep facts must not gain an expiry the store cannot represent")
	}
}

func TestParseGenericJSONL(t *testing.T) {
	res, err := Parse(fixture(t, "generic.jsonl"), "jsonl-generic")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 3 || len(res.Skips) != 1 {
		t.Fatalf("items=%d skips=%d", len(res.Items), len(res.Skips))
	}
	if res.Items[0].Record.Agent != "ops" || res.Items[0].Record.Category != "procedure" {
		t.Errorf("first = %+v", res.Items[0].Record)
	}
	if res.Items[2].Record.Stamp != "2026-04-04 12:30" {
		t.Errorf("stamp = %q", res.Items[2].Record.Stamp)
	}
}

func TestParseGrimoireRestoresFieldsAndDropsAuthority(t *testing.T) {
	res, err := Parse(fixture(t, "grimoire.jsonl"), "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != SourceGrimoire || len(res.Items) != 2 || len(res.Skips) != 1 {
		t.Fatalf("source=%s items=%d skips=%+v", res.Source, len(res.Items), res.Skips)
	}
	rec := res.Items[1].Record
	if !res.Items[1].Restored || rec.ID != "ffee00112233" || rec.Origin != "connector:slack:C1" ||
		rec.SupersededBy != "a1b2c3d4e5f6" || !rec.Immutable || rec.Expires != "2027-01-01T00:00:00Z" {
		t.Errorf("restored record lost fields: %+v", rec)
	}
	if res.Items[0].Record.Authority != "" {
		t.Error("authority is informational on import and must not be trusted from the file")
	}
	if res.Skips[0].Reason == "" {
		t.Error("a bad stamp must be reported, not written")
	}
}

func TestGrimoireVersionIsChecked(t *testing.T) {
	_, err := Parse([]byte(`{"format":"grimoire-memory","version":2,"count":0}`+"\n"), "grimoire")
	if err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("expected a version refusal, got %v", err)
	}
}

func TestWriteJSONLRoundTripsThroughParse(t *testing.T) {
	in := []Record{
		{ID: "abc", Text: "one", Path: "memory/x.md", Agent: "a", Stamp: "2026-08-14 09:00", Authority: "agent"},
		{ID: "def", Text: "two \"quoted\"\nline", Path: "memory/x.md", Category: "fact", Stamp: "2026-08-14 09:01",
			SupersededBy: "abc", Challenges: "abc"},
	}
	out := WriteJSONL(in, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC))
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], `{"format":"grimoire-memory","version":1`) {
		t.Fatalf("export = %q", out)
	}
	res, err := Parse(out, "grimoire")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 || res.Items[1].Record.Text != in[1].Text ||
		res.Items[1].Record.SupersededBy != "abc" || res.Items[1].Record.Challenges != "abc" {
		t.Fatalf("round trip = %+v", res.Items)
	}
}

func TestWriteMarkdownGroupsAndStrikesSuperseded(t *testing.T) {
	out := string(WriteMarkdown([]Record{
		{ID: "1", Text: "old belief", Category: "fact", SupersededBy: "2", Stamp: "2026-08-01 10:00", Agent: "cli"},
		{ID: "2", Text: "new belief", Category: "fact", Authority: "human"},
		{ID: "3", Text: "prefers tabs", Category: "preference"},
	}, time.Date(2026, 8, 14, 9, 0, 0, 0, time.UTC)))
	for _, want := range []string{"## fact", "## preference", "~~old belief~~ (superseded by 2)",
		"- new belief  _(human)_"} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
	if strings.Index(out, "## fact") > strings.Index(out, "## preference") {
		t.Error("categories must be sorted")
	}
}

func mustLocalStamp(t *testing.T, rfc string) string {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, rfc)
	if err != nil {
		t.Fatal(err)
	}
	return ts.Local().Format(StampLayout)
}
