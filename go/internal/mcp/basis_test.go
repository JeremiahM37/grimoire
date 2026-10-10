package mcp

import (
	"encoding/json"
	"strings"
	"testing"
)

// The recall text block leads each fact with its basis, so a model can read
// "stated" versus "inferred" without parsing the JSON that follows it.
func TestRecallLinesLeadEachFactWithItsBasis(t *testing.T) {
	result := []any{
		map[string]any{"id": "aaa111", "basis": "stated", "text": "the owner is  Jeremiah"},
		map[string]any{"id": "bbb222", "basis": "observed", "text": "the backup runs at 03:00"},
	}
	want := "- [stated] the owner is Jeremiah (aaa111)\n- [observed] the backup runs at 03:00 (bbb222)"
	if got := recallLines(result); got != want {
		t.Fatalf("recallLines =\n%s\nwant\n%s", got, want)
	}
}

func TestRecallLinesAddsNothingForAFailedOrEmptyRecall(t *testing.T) {
	for _, result := range []any{nil, map[string]any{"error": "boom"}, []any{}} {
		if got := recallLines(result); got != "" {
			t.Errorf("recallLines(%v) = %q, want empty", result, got)
		}
	}
}

// The new arguments must reach the tool schema, or an agent can never send them.
func TestBasisModeEvidenceAndExcludePersonalAreInTheSchemas(t *testing.T) {
	schemas := map[string]string{}
	for _, tl := range Tools() {
		raw, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		schemas[tl.Name] = string(raw)
	}
	for tool, field := range map[string]string{
		"recall": `"basis"`, "remember": `"evidence"`, "memory_profile": `"exclude_personal"`,
	} {
		if !strings.Contains(schemas[tool], field) {
			t.Errorf("%s schema has no %s", tool, field)
		}
	}
	if !strings.Contains(schemas["recall"], `"mode"`) {
		t.Error("recall schema has no mode")
	}
}
