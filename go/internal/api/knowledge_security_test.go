package api

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
)

func responseJSON(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response was not JSON: %d %s: %v", w.Code, w.Body, err)
	}
	return out
}

func containsJSONText(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return strings.Contains(x, want)
	case []any:
		for _, item := range x {
			if containsJSONText(item, want) {
				return true
			}
		}
	case map[string]any:
		for _, item := range x {
			if containsJSONText(item, want) {
				return true
			}
		}
	}
	return false
}

func writeSecurityNote(t *testing.T, s *Server, path, body string, frontmatter map[string]any) {
	t.Helper()
	if _, err := s.WriteNote(path, body, frontmatter); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func multipartUpload(t *testing.T, h http.Handler, key, path, filename, body string) *httptest.ResponseRecorder {
	t.Helper()
	var data bytes.Buffer
	mw := multipart.NewWriter(&data)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(part, body); err != nil {
		t.Fatal(err)
	}
	if path != "" {
		if err := mw.WriteField("path", path); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/documents/import", &data)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+key)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestKnowledgeSurfacesHidePrivateSpaceAndReaderNotes(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")

	writeSecurityNote(t, s, "public.md", "# Public\n\nvisible marker", map[string]any{"entities": []string{"PublicEntity"}})
	writeSecurityNote(t, s, "private.md", "# Private\n\nPRIVATE_MARKER evidence", map[string]any{"private": true, "entities": []string{"PrivateEntity"}})
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	writeSecurityNote(t, s, "reader.md", "# Reader\n\nREADER_MARKER evidence", map[string]any{"readers": alice.ID, "entities": []string{"ReaderEntity"}})
	space := asKey(t, h, adminKey, http.MethodPost, "/api/spaces", map[string]any{"name": "Secret", "prefix": "team/secret"})
	if space.Code != http.StatusCreated {
		t.Fatalf("create space = %d %s", space.Code, space.Body)
	}
	writeSecurityNote(t, s, "team/secret/runbook.md", "# Space\n\nSPACE_MARKER evidence", map[string]any{"entities": []string{"SpaceEntity"}})

	for _, tc := range []struct {
		name string
		key  string
	}{
		{"alice", aliceKey},
		{"bob", bobKey},
	} {
		w := asKey(t, h, tc.key, http.MethodGet, "/api/knowledge/graph", nil)
		if w.Code != http.StatusOK {
			t.Fatalf("graph = %d %s", w.Code, w.Body)
		}
		graph := responseJSON(t, w)
		if containsJSONText(graph, "PRIVATE_MARKER") || containsJSONText(graph, "SPACE_MARKER") {
			t.Errorf("restricted text leaked through graph for %s: %s", tc.name, w.Body)
		}
		if tc.name == "bob" && containsJSONText(graph, "READER_MARKER") {
			t.Errorf("reader-only text leaked through graph for bob: %s", w.Body)
		}
	}

	for _, hidden := range []string{"private.md", "reader.md", "team/secret/runbook.md"} {
		w := asKey(t, h, bobKey, http.MethodGet, "/api/knowledge/source?path="+hidden, nil)
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "MARKER") {
			t.Errorf("bob source %s = %d %s", hidden, w.Code, w.Body)
		}
	}
	w := asKey(t, h, bobKey, http.MethodPost, "/api/knowledge/query", map[string]any{"question": "evidence", "limit": 20})
	if w.Code != http.StatusOK {
		t.Fatalf("query = %d %s", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "PRIVATE_MARKER") || strings.Contains(w.Body.String(), "READER_MARKER") || strings.Contains(w.Body.String(), "SPACE_MARKER") {
		t.Errorf("restricted text leaked through query: %s", w.Body)
	}

	if w := asKey(t, h, aliceKey, http.MethodGet, "/api/knowledge/source?path=reader.md", nil); w.Code != http.StatusOK {
		t.Fatalf("alice could not read reader note: %d %s", w.Code, w.Body)
	}
}

func TestKnowledgeTrustedOnlyFiltersBeforeGraphStats(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	writeSecurityNote(t, s, "trusted.md", "# Trusted\n\nTRUSTED_MARKER", map[string]any{"entities": []string{"TrustedEntity"}})
	writeSecurityNote(t, s, "pulled.md", "# Pulled\n\nUNTRUSTED_MARKER", map[string]any{"origin": "connector:feed:C1", "entities": []string{"UntrustedEntity"}})

	all := asKey(t, h, adminKey, http.MethodGet, "/api/knowledge/graph", nil)
	if all.Code != http.StatusOK || !strings.Contains(all.Body.String(), "UNTRUSTED_MARKER") {
		t.Fatalf("unfiltered graph did not contain fixture: %d %s", all.Code, all.Body)
	}
	trusted := asKey(t, h, adminKey, http.MethodGet, "/api/knowledge/graph?trusted=1", nil)
	if trusted.Code != http.StatusOK {
		t.Fatalf("trusted graph = %d %s", trusted.Code, trusted.Body)
	}
	if strings.Contains(trusted.Body.String(), "UNTRUSTED_MARKER") || !strings.Contains(trusted.Body.String(), "TRUSTED_MARKER") {
		t.Errorf("trusted graph did not filter evidence before assembly: %s", trusted.Body)
	}
	var allGraph, trustedGraph map[string]any
	decode(t, all, &allGraph)
	decode(t, trusted, &trustedGraph)
	allStats, _ := allGraph["stats"].(map[string]any)
	trustedStats, _ := trustedGraph["stats"].(map[string]any)
	if allStats != nil && trustedStats != nil && trustedStats["documents"].(float64) >= allStats["documents"].(float64) {
		t.Errorf("trusted graph stats counted hidden document: all=%v trusted=%v", allStats, trustedStats)
	}
}

func TestKnowledgeExtractionIsMultiuserScopedBeforeModelCalls(t *testing.T) {
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"response": `[{"subject":"Public Subject","relation":"relates","object":"Public Object","quote":"Public Subject relates Public Object"}]`})
	}))
	defer model.Close()

	s, h := testServer(t)
	s.AI = ai.New(stubSettings{"llm": "ollama", "ollama_url": model.URL}, nil)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	aliceKey := makeUser(t, s, h, adminKey, "alice", "member")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	alice, err := s.Auth.ByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	writeSecurityNote(t, s, "public.md", "Public Subject relates Public Object", nil)
	writeSecurityNote(t, s, "private.md", "Private Subject relates Private Object", map[string]any{"private": true})
	writeSecurityNote(t, s, "reader.md", "Reader Subject relates Reader Object", map[string]any{"readers": alice.ID})
	space := asKey(t, h, adminKey, http.MethodPost, "/api/spaces", map[string]any{"name": "Secret", "prefix": "team/secret"})
	if space.Code != http.StatusCreated {
		t.Fatalf("create space = %d %s", space.Code, space.Body)
	}
	writeSecurityNote(t, s, "team/secret/runbook.md", "Space Subject relates Space Object", nil)

	anonymous := do(t, h, http.MethodPost, "/api/knowledge/extract", map[string]any{"paths": []string{"public.md"}})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous extraction = %d %s", anonymous.Code, anonymous.Body)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("anonymous extraction invoked model %d times", got)
	}

	response := asKey(t, h, bobKey, http.MethodPost, "/api/knowledge/extract", map[string]any{"paths": []string{
		"public.md", "private.md", "reader.md", "team/secret/runbook.md",
	}})
	if response.Code != http.StatusOK {
		t.Fatalf("bob extraction = %d %s", response.Code, response.Body)
	}
	var result struct {
		Results []struct {
			Path    string `json:"path"`
			Status  string `json:"status"`
			Error   string `json:"error"`
			Triples int    `json:"triples"`
		} `json:"results"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 4 {
		t.Fatalf("unexpected extraction result: %s", response.Body)
	}
	if result.Results[0].Path != "public.md" || result.Results[0].Status != "indexed" || result.Results[0].Triples != 1 {
		t.Fatalf("public extraction result = %+v", result.Results[0])
	}
	for _, got := range result.Results[1:] {
		if got.Status != "error" || got.Error != "source is not accessible" {
			t.Errorf("hidden extraction result for %s = %+v", got.Path, got)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("Bob hidden batch invoked model %d times, want 1 public call", got)
	}

	aliceResponse := asKey(t, h, aliceKey, http.MethodPost, "/api/knowledge/extract", map[string]any{"paths": []string{"reader.md"}})
	if aliceResponse.Code != http.StatusOK || !strings.Contains(aliceResponse.Body.String(), `"status":"indexed"`) {
		t.Fatalf("Alice authorized reader extraction = %d %s", aliceResponse.Code, aliceResponse.Body)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("authorized reader extraction made %d total model calls, want 2", got)
	}
}

func TestDocumentsEnforceReaderWriteBoundariesAndHideOriginals(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	bobKey := makeUser(t, s, h, adminKey, "bob", "member")
	space := asKey(t, h, adminKey, http.MethodPost, "/api/spaces", map[string]any{"name": "Documents", "prefix": "documents"})
	if space.Code != http.StatusCreated {
		t.Fatalf("create documents space = %d %s", space.Code, space.Body)
	}
	var spaceBody map[string]any
	decode(t, space, &spaceBody)
	if w := asKey(t, h, adminKey, http.MethodPost, "/api/spaces/"+spaceBody["id"].(string)+"/members", map[string]any{"user": "bob", "role": "reader"}); w.Code != http.StatusOK {
		t.Fatalf("add bob as document reader = %d %s", w.Code, w.Body)
	}

	created := multipartUpload(t, h, adminKey, "", "secret.txt", "ORIGINAL_SECRET")
	if created.Code != http.StatusCreated {
		t.Fatalf("import = %d %s", created.Code, created.Body)
	}
	var result map[string]any
	decode(t, created, &result)
	path := result["path"].(string)
	if !strings.HasPrefix(path, "documents/") {
		t.Fatalf("import escaped documents space: %s", path)
	}
	privateCreated := multipartUpload(t, h, adminKey, "", "hidden.txt", "HIDDEN_ORIGINAL_SECRET")
	if privateCreated.Code != http.StatusCreated {
		t.Fatalf("private import = %d %s", privateCreated.Code, privateCreated.Body)
	}
	var privateResult map[string]any
	decode(t, privateCreated, &privateResult)
	privatePath := privateResult["path"].(string)
	note, err := s.Vault.Read(privatePath)
	if err != nil {
		t.Fatal(err)
	}
	fm := note.Frontmatter.Clone()
	fm.Set("private", true)
	if _, err := s.Vault.Write(privatePath, note.Body, fm); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Index.Upsert(privatePath); err != nil {
		t.Fatal(err)
	}

	if w := asKey(t, h, bobKey, http.MethodGet, "/api/documents", nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), path) || strings.Contains(w.Body.String(), privatePath) {
		t.Errorf("bob document list had incorrect visibility: %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, http.MethodGet, "/api/documents/original?path="+privatePath, nil); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "HIDDEN_ORIGINAL_SECRET") {
		t.Errorf("bob fetched hidden original: %d %s", w.Code, w.Body)
	}
	w := multipartUpload(t, h, bobKey, path, "replacement.txt", "BOB_REPLACEMENT")
	if w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Errorf("read-only bob replaced document: %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, http.MethodPost, "/api/documents/refresh", map[string]any{"path": path}); w.Code == http.StatusOK {
		t.Errorf("read-only bob refreshed document: %d %s", w.Code, w.Body)
	}
	if w := multipartUpload(t, h, bobKey, "", "new.txt", "BOB_NEW"); w.Code == http.StatusCreated || w.Code == http.StatusOK {
		t.Errorf("read-only bob imported document: %d %s", w.Code, w.Body)
	}
	if w := asKey(t, h, bobKey, http.MethodGet, "/api/documents/original?path="+path, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ORIGINAL_SECRET") {
		t.Errorf("bob could not fetch readable original: %d %s", w.Code, w.Body)
	}
}

func TestKnowledgeAndDocumentPathsRejectTraversalAndSymlinkSources(t *testing.T) {
	s, h := testServer(t)
	adminKey := makeUser(t, s, h, "", "admin", "admin")
	for _, path := range []string{"../.grimoire/index.db", "/etc/passwd", "documents/../../outside.md"} {
		if w := asKey(t, h, adminKey, http.MethodGet, "/api/knowledge/source?path="+path, nil); w.Code == http.StatusOK {
			t.Errorf("source traversal succeeded for %q: %s", path, w.Body)
		}
		if w := asKey(t, h, adminKey, http.MethodGet, "/api/documents/original?path="+path, nil); w.Code == http.StatusOK {
			t.Errorf("original traversal succeeded for %q: %s", path, w.Body)
		}
	}
	outside := filepath.Join(t.TempDir(), "source.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "source-link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Documents.ImportFile(link); err == nil {
		t.Fatal("document importer accepted a symlink source")
	}
}
