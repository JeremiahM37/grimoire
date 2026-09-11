package api

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type noContextEmbedding struct{ testing *testing.T }

func (embedding noContextEmbedding) Embed(texts []string) [][]float32 {
	embedding.testing.Fatal("automatic context must not call an embedding model")
	return nil
}

func (noContextEmbedding) Signature() string { return "no-context-embedding" }
func (noContextEmbedding) Dim() int          { return 384 }

func TestMemoryContextRelevantBoundedAndModelFree(t *testing.T) {
	server, handler := testServer(t)
	remember(t, handler, map[string]any{"text": "The kestrel deployment requires copper certificates", "topic": "kestrel"})
	remember(t, handler, map[string]any{"text": "The kitchen cupboards are painted purple", "topic": "kitchen"})
	server.Index.Emb = noContextEmbedding{t}
	response := do(t, handler, "GET", "/api/memory/context?q=kestrel+deployment&max_bytes=900", nil)
	var result map[string]any
	decode(t, response, &result)
	context := result["context"].(string)
	if !strings.Contains(context, "copper certificates") || strings.Contains(context, "purple") || len(context) > 900 {
		t.Fatalf("bad context: %s", context)
	}
	key := result["keys"].([]any)[0].(string)
	response = do(t, handler, "GET", "/api/memory/context?q=kestrel+deployment&exclude="+key, nil)
	decode(t, response, &result)
	if result["context"] != "" {
		t.Fatalf("duplicate context: %v", result)
	}
	for _, query := range []string{"continue", "thanks", "purple galaxies", ""} {
		response = do(t, handler, "GET", "/api/memory/context?q="+url.QueryEscape(query), nil)
		decode(t, response, &result)
		if result["context"] != "" {
			t.Fatalf("irrelevant %q: %v", query, result)
		}
	}
}

func TestMemoryContextExcludesChallengesAndRecallsCorrectedText(t *testing.T) {
	_, handler := testServer(t)
	first := remember(t, handler, map[string]any{"text": "The kestrel deployment uses copper certificates", "topic": "kestrel", "human": true})
	remember(t, handler, map[string]any{"text": "The kestrel deployment uses silver certificates", "topic": "kestrel"})
	response := do(t, handler, "GET", "/api/memory/context?q=kestrel+deployment", nil)
	if strings.Contains(response.Body.String(), "silver") || !strings.Contains(response.Body.String(), "copper") {
		t.Fatal(response.Body.String())
	}
	var before map[string]any
	decode(t, response, &before)
	patch := do(t, handler, "PATCH", "/api/memory/entry", map[string]any{"path": first["path"], "id": first["id"], "text": "The kestrel deployment uses gold certificates"})
	if patch.Code != 200 {
		t.Fatal(patch.Body.String())
	}
	response = do(t, handler, "GET", "/api/memory/context?q=kestrel+deployment&exclude="+before["keys"].([]any)[0].(string), nil)
	if !strings.Contains(response.Body.String(), "gold") {
		t.Fatal(response.Body.String())
	}
}

func TestMemoryContextIncludesNotesButNotPrivateImportedOrHistory(t *testing.T) {
	_, handler := testServer(t)
	for _, note := range []map[string]any{
		{"path": "runbook.md", "body": "# Kestrel deployment\n\nKestrel deployment needs bronze certificates."},
		{"path": "private.md", "body": "Kestrel deployment HIDDEN_SECRET", "frontmatter": map[string]any{"private": true}},
		{"path": "pulled.md", "body": "Kestrel deployment INJECTED_TEXT", "frontmatter": map[string]any{"origin": "connector:slack"}},
	} {
		response := do(t, handler, "POST", "/api/notes", note)
		if response.Code != http.StatusCreated {
			t.Fatal(response.Body.String())
		}
	}
	remember(t, handler, map[string]any{"text": "Kestrel deployment uses OLD_CERTIFICATES", "topic": "kestrel"})
	remember(t, handler, map[string]any{"text": "Kestrel deployment uses NEW_CERTIFICATES", "topic": "kestrel"})
	response := do(t, handler, "GET", "/api/memory/context?q=kestrel+deployment", nil)
	for _, forbidden := range []string{"HIDDEN_SECRET", "INJECTED_TEXT", "OLD_CERTIFICATES"} {
		if strings.Contains(response.Body.String(), forbidden) {
			t.Fatal(response.Body.String())
		}
	}
	if !strings.Contains(response.Body.String(), "bronze") {
		t.Fatal(response.Body.String())
	}
}

func TestExplicitCorrectionBypassesParaphraseRecognition(t *testing.T) {
	_, handler := testServer(t)
	first := remember(t, handler, map[string]any{"topic": "deploy", "text": "The team sits downstairs"})
	replacement := map[string]any{"topic": "deploy", "text": "Our new office is on floor seven",
		"target_id": first["id"], "target_path": first["path"], "expected_text": "The team sits downstairs"}
	result := remember(t, handler, replacement)
	if result["op"] != "UPDATE" {
		t.Fatal(result)
	}
	response := do(t, handler, "POST", "/api/memory", replacement)
	if response.Code != http.StatusNotFound {
		t.Fatalf("stale correction: %d %s", response.Code, response.Body)
	}
}

func TestExplicitCorrectionProtectsHumanAndRejectsStaleText(t *testing.T) {
	_, handler := testServer(t)
	first := remember(t, handler, map[string]any{"topic": "deploy", "text": "The team sits downstairs", "human": true})
	replacement := map[string]any{"topic": "deploy", "text": "Our new office is on floor seven",
		"target_id": first["id"], "target_path": first["path"], "expected_text": "The team sits downstairs"}
	result := remember(t, handler, replacement)
	if result["results"].([]any)[0].(map[string]any)["challenges"] != first["id"] {
		t.Fatal(result)
	}
	replacement["expected_text"] = "wrong old text"
	response := do(t, handler, "POST", "/api/memory", replacement)
	if response.Code != http.StatusConflict {
		t.Fatalf("stale correction: %d %s", response.Code, response.Body)
	}
}

func TestMemoryContextScopeIsAnAllowlistNotAQueryHint(t *testing.T) {
	server, handler := testServer(t)
	for number := 0; number < 110; number++ {
		remember(t, handler, map[string]any{"text": fmt.Sprintf("Kestrel deployment forbidden %d", number), "topic": "other", "infer": false})
	}
	remember(t, handler, map[string]any{"text": "Kestrel deployment accepted copper", "topic": "kestrel", "infer": false})
	writeSecurityNote(t, server, "projects/kestrel/runbook.md", "Kestrel deployment nested allowed", nil)
	writeSecurityNote(t, server, "projects/kestrel-other/runbook.md", "Kestrel deployment neighbor forbidden", nil)
	endpoint := "/api/memory/context?scope=scoped&path=memory/kestrel.md&path=projects/kestrel/"
	for _, query := range []string{"", "&q=kestrel+deployment"} {
		response := do(t, handler, "GET", endpoint+query, nil)
		if response.Code != 200 || strings.Contains(response.Body.String(), "forbidden") || !strings.Contains(response.Body.String(), "accepted copper") || !strings.Contains(response.Body.String(), "nested allowed") {
			t.Fatalf("scope leaked or missed: %d %s", response.Code, response.Body)
		}
	}
	for _, query := range []string{"scope=scoped", "scope=scoped&path=../", "scope=scoped&path=/", "scope=all&path=memory/kestrel.md"} {
		response := do(t, handler, "GET", "/api/memory/context?"+query, nil)
		if response.Code != 400 {
			t.Fatalf("unsafe scope: %d %s", response.Code, response.Body)
		}
	}
}

func TestMemoryContextScopeDoesNotGrantAccess(t *testing.T) {
	server, handler := testServer(t)
	adminKey := makeUser(t, server, handler, "", "admin", "admin")
	aliceKey := makeUser(t, server, handler, adminKey, "alice", "member")
	bobKey := makeUser(t, server, handler, adminKey, "bob", "member")
	alice, _ := server.Auth.ByName("alice")
	writeSecurityNote(t, server, "reader.md", "Kestrel deployment HIDDEN_READER", map[string]any{"readers": alice.ID})
	writeSecurityNote(t, server, "memory/reader.md", "# Memory\n\n- Kestrel deployment HIDDEN_FACT", map[string]any{"readers": alice.ID})
	endpoint := "/api/memory/context?scope=scoped&path=reader.md&path=memory/reader.md&q=kestrel+deployment"
	for _, key := range []string{"", bobKey} {
		response := asKey(t, handler, key, "GET", endpoint, nil)
		if strings.Contains(response.Body.String(), "HIDDEN") {
			t.Fatalf("leaked: %s", response.Body)
		}
	}
	response := asKey(t, handler, aliceKey, "GET", endpoint, nil)
	if !strings.Contains(response.Body.String(), "HIDDEN_READER") {
		t.Fatal(response.Body.String())
	}
}

func TestExplicitCorrectionSurvivesReindexAndDoesNotTrustStaleIndex(t *testing.T) {
	server, handler := testServer(t)
	first := remember(t, handler, map[string]any{"topic": "deploy", "text": "The team sits downstairs"})
	notePath := first["path"].(string)
	note, _ := server.Vault.Read(notePath)
	if _, err := server.Vault.Write(notePath, strings.ReplaceAll(note.Body, "downstairs", "upstairs"), note.Frontmatter); err != nil {
		t.Fatal(err)
	}
	response := do(t, handler, "POST", "/api/memory", map[string]any{
		"text": "Our office moved to floor seven", "target_id": first["id"], "target_path": notePath, "expected_text": "The team sits downstairs"})
	if response.Code != 409 {
		t.Fatalf("stale index accepted: %d %s", response.Code, response.Body)
	}
	if _, err := server.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	result := remember(t, handler, map[string]any{
		"text": "Our office moved to floor seven", "target_id": first["id"], "target_path": notePath, "expected_text": "The team sits upstairs"})
	if result["op"] != "ADD" || result["results"].([]any)[0].(map[string]any)["challenges"] != first["id"] {
		t.Fatal(result)
	}
	if _, err := server.Index.Reindex(); err != nil {
		t.Fatal(err)
	}
	facts := recallFacts(t, handler, "")
	if len(facts) != 1 || facts[0]["text"] != "The team sits upstairs" {
		t.Fatal(facts)
	}
}
