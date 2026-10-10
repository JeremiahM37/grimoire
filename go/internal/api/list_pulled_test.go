package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

// A connector sync lands many items at once, all newer than anything written
// by hand. The capped list must still lead with the written notes.
func TestListKeepsWrittenNotesAheadOfPulledItems(t *testing.T) {
	_, h := testServer(t)
	if w := do(t, h, "POST", "/api/notes", map[string]any{"path": "mine.md", "body": "# Mine"}); w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	for i := 0; i < 5; i++ {
		path := fmt.Sprintf("connectors/mail/item-%d.md", i)
		if w := do(t, h, "POST", "/api/notes", map[string]any{"path": path, "body": "# Mail"}); w.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", path, w.Code, w.Body)
		}
	}
	w := do(t, h, "GET", "/api/notes?limit=3", nil)
	var items []struct{ Path string }
	if err := json.Unmarshal(w.Body.Bytes(), &items); err != nil {
		t.Fatal(err)
	}
	if len(items) != 3 || items[0].Path != "mine.md" {
		t.Fatalf("want mine.md first of 3, got %+v", items)
	}
}
