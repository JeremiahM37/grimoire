package ai

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mapSettings is a settings store with no file behind it.
type mapSettings map[string]string

func (m mapSettings) Get(k string) string { return m[k] }

// fakeOllama answers /api/generate with a canned completion and records what
// was asked, so the prompt contract can be asserted rather than assumed.
func fakeOllama(t *testing.T, reply string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var seen []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"response": reply})
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

func TestBackendAutoEnablesWithOllama(t *testing.T) {
	if got := New(mapSettings{}, nil).Backend(); got != "" {
		t.Errorf("backend with nothing configured = %q, want extractive", got)
	}
	// the common self-hosted deployment sets only GRIMOIRE_OLLAMA_URL, so that
	// alone has to turn generative answers on
	if got := New(mapSettings{"ollama_url": "http://x:11434"}, nil).Backend(); got != "ollama" {
		t.Errorf("backend = %q, want ollama", got)
	}
	// an explicit choice still wins
	if got := New(mapSettings{"llm": "openai", "ollama_url": "http://x"}, nil).Backend(); got != "openai" {
		t.Errorf("backend = %q, want openai", got)
	}
}

func TestAnswerUsesTheLLMAndCitesSources(t *testing.T) {
	srv, seen := fakeOllama(t, "The port is 8443 [1].")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.Answer("what port", []Context{
		{Path: "ops.md", Title: "Ops", Chunk: "port:: 8443"}})
	if got != "The port is 8443 [1]." {
		t.Errorf("answer = %q", got)
	}
	prompt := (*seen)[0]["prompt"].(string)
	if !strings.Contains(prompt, "ONLY the notes below") || !strings.Contains(prompt, "port:: 8443") {
		t.Errorf("prompt lost its grounding instruction or context:\n%s", prompt)
	}
	if (*seen)[0]["think"] != false {
		t.Error("think must be false — a reasoning model otherwise burns seconds here")
	}
}

func TestAnswerFallsBackToExtractiveWhenTheLLMFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "model not found", http.StatusInternalServerError)
	}))
	defer srv.Close()

	// an LLM outage must never fail the request — it must quietly produce the
	// offline answer instead
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.Answer("what port", []Context{
		{Path: "ops.md", Title: "Ops", Chunk: "port:: 8443"}})
	if !strings.Contains(got, "port:: 8443") || !strings.Contains(got, "[[ops|Ops]]") {
		t.Errorf("answer = %q, want the extractive form with a citation", got)
	}
}

func TestExtractiveAnswerTruncatesLongPassages(t *testing.T) {
	long := strings.Repeat("word ", 300)
	got := ExtractiveAnswer("q", []Context{{Path: "a.md", Title: "A", Chunk: long}})
	if !strings.Contains(got, " …") {
		t.Error("a long passage was not truncated")
	}
	if len([]rune(got)) > 600 {
		t.Errorf("extractive answer is %d chars — the budget is per passage", len([]rune(got)))
	}
}

func TestDecomposeOnlyRunsOnQuestionsWorthSplitting(t *testing.T) {
	srv, seen := fakeOllama(t, "Where does A live?\nWhere does B live?")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)

	if got := c.Decompose("short question"); len(got) != 1 {
		t.Errorf("a short question was decomposed: %v", got)
	}
	if len(*seen) != 0 {
		t.Error("a short question still called the LLM")
	}

	got := c.Decompose("where do A and B live and how do they talk to each other")
	if len(got) != 2 || got[0] != "Where does A live?" {
		t.Errorf("decompose = %v", got)
	}
}

func TestDecomposeSurvivesAGarbledReply(t *testing.T) {
	srv, _ := fakeOllama(t, "\n\n")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	q := "where do A and B live and how do they talk to each other"
	if got := c.Decompose(q); len(got) != 1 || got[0] != q {
		t.Errorf("decompose = %v, want the original question back", got)
	}
}

func TestRerankKeepsUnmentionedCandidates(t *testing.T) {
	srv, _ := fakeOllama(t, "2")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	in := []Context{{Chunk: "zero"}, {Chunk: "one"}, {Chunk: "two"}}

	// a partial reply must degrade, not drop passages the model forgot
	got := c.Rerank("q", in, 3)
	if len(got) != 3 {
		t.Fatalf("got %d passages, want all 3 back", len(got))
	}
	if got[0].Chunk != "two" || got[1].Chunk != "zero" || got[2].Chunk != "one" {
		t.Errorf("order = %v", []string{got[0].Chunk, got[1].Chunk, got[2].Chunk})
	}
}

func TestRerankIsANoOpWithoutAnLLM(t *testing.T) {
	c := New(mapSettings{}, nil)
	in := []Context{{Chunk: "a"}, {Chunk: "b"}, {Chunk: "c"}}
	got := c.Rerank("q", in, 2)
	if len(got) != 2 || got[0].Chunk != "a" || got[1].Chunk != "b" {
		t.Errorf("rerank without an LLM changed the order: %v", got)
	}
}

func TestConsolidateRejectsADegenerateRewrite(t *testing.T) {
	body := "# Memory\n\n- **one** — first\n- **one** — first\n- **two** — second\n"

	// a model that returns almost nothing would silently destroy the memory
	srv, _ := fakeOllama(t, "ok")
	c := New(mapSettings{"ollama_url": srv.URL}, nil)
	got := c.ConsolidateMemory(body)
	if strings.TrimSpace(got) == "ok" {
		t.Fatal("a degenerate rewrite was accepted")
	}
	if strings.Count(got, "- **one** — first") != 1 {
		t.Errorf("fallback did not dedupe:\n%s", got)
	}
	if !strings.Contains(got, "- **two** — second") {
		t.Errorf("fallback lost an entry:\n%s", got)
	}
}

func TestDedupLinesPreservesNonEntries(t *testing.T) {
	in := "# Heading\n\n- a\n- a\nprose\nprose\n- b\n"
	got := DedupLines(in)
	if strings.Count(got, "- a") != 1 {
		t.Errorf("duplicate entry survived:\n%s", got)
	}
	// only '- ' entries are deduped; repeated prose is not a duplicate entry
	if strings.Count(got, "prose") != 2 {
		t.Errorf("non-entry lines were deduped:\n%s", got)
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("trailing newline lost")
	}
}

func TestTranscribeSavesAPlaceholderWithoutAService(t *testing.T) {
	c := New(mapSettings{}, nil)
	if got := c.Transcribe([]byte("audio"), "memo.webm"); !strings.Contains(got, "unavailable") {
		t.Errorf("transcript = %q", got)
	}
}

func TestTranscribeUsesAWhisperService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			t.Errorf("content type = %q", r.Header.Get("Content-Type"))
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"text": "hello there"})
	}))
	defer srv.Close()
	c := New(mapSettings{"whisper_url": srv.URL}, nil)
	if got := c.Transcribe([]byte("audio"), "memo.webm"); got != "hello there" {
		t.Errorf("transcript = %q", got)
	}
}

func TestSuggestTagsRanksByFrequencyThenFirstSeen(t *testing.T) {
	got := SuggestTags("deploy deploy runbook rollback proxy this that with")
	if len(got) == 0 || got[0] != "deploy" {
		t.Fatalf("tags = %v", got)
	}
	// ties keep first-seen order, matching Counter.most_common
	if strings.Join(got, ",") != "deploy,runbook,rollback,proxy" {
		t.Errorf("tags = %v", got)
	}
}

func TestFirstLineTitleSkipsBlanksAndHashes(t *testing.T) {
	if got := FirstLineTitle("\n\n#  Real Title\nbody"); got != "Real Title" {
		t.Errorf("title = %q", got)
	}
	if got := FirstLineTitle(""); got != "Untitled" {
		t.Errorf("title = %q", got)
	}
}

func TestAPIKeyFallsBackToTheCredentialVault(t *testing.T) {
	c := New(mapSettings{"llm": "openai"}, func(name string) (string, error) {
		if name == "llm-api-key" {
			return "sk-from-vault", nil
		}
		return "", http.ErrNoLocation
	})
	if got := c.apiKey(); got != "sk-from-vault" {
		t.Errorf("apiKey = %q", got)
	}
	// an explicit setting still wins over the vault
	c2 := New(mapSettings{"llm_api_key": "sk-explicit"}, func(string) (string, error) {
		return "sk-from-vault", nil
	})
	if got := c2.apiKey(); got != "sk-explicit" {
		t.Errorf("apiKey = %q", got)
	}
}
