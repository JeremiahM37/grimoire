// Package ai is the optional LLM layer: answer synthesis, question
// decomposition, reranking, memory consolidation and audio transcription.
//
// Port of the answer-synthesis half of server/ai.py (the embedding half lives
// in internal/embed). The design rule carried over intact: grimoire works fully
// offline with no external dependency, and gets *smarter* — not merely
// functional — when pointed at a local Ollama or a hosted model. Every entry
// point here has a deterministic fallback, and an LLM outage must never fail a
// request; it must quietly produce the offline answer instead.
package ai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"unicode/utf8"
)

// Settings is the subset of the settings store this package reads. An
// interface so tests can drive the backends without a file on disk.
type Settings interface {
	Get(key string) string
}

// SecretGetter fetches a named secret from the credential vault, used only to
// dogfood the vault for the LLM API key. Nil is fine — it just means the key
// must come from settings or the environment.
type SecretGetter func(name string) (string, error)

// Client holds the configuration lookups. It is stateless otherwise, so a
// settings change takes effect on the next call with no restart.
type Client struct {
	Settings Settings
	Secret   SecretGetter
	HTTP     *http.Client
}

func New(st Settings, secret SecretGetter) *Client {
	return &Client{Settings: st, Secret: secret,
		HTTP: &http.Client{Timeout: 120 * time.Second}}
}

func (c *Client) get(key string) string {
	if c == nil || c.Settings == nil {
		return ""
	}
	return c.Settings.Get(key)
}

func (c *Client) ollamaURL() string { return strings.TrimRight(c.get("ollama_url"), "/") }
func (c *Client) model() string {
	if m := c.get("llm_model"); m != "" {
		return m
	}
	return "qwen3.5:4b"
}

// apiKey resolves the OpenAI-compatible key: explicit setting/env first, then
// the credential vault (an unlocked secret named 'llm-api-key') so the key is
// stored the same audited way agents' secrets are. Local servers (vLLM, LM
// Studio, Ollama's OpenAI shim) usually need no key at all.
func (c *Client) apiKey() string {
	if k := c.get("llm_api_key"); k != "" {
		return k
	}
	if c.Secret != nil {
		if v, err := c.Secret("llm-api-key"); err == nil {
			return v
		}
	}
	return ""
}

// Backend reports which LLM (if any) synthesizes answers. An explicit `llm`
// setting wins; otherwise a reachable Ollama AUTO-enables generative answers,
// because the common self-hosted deployment sets only GRIMOIRE_OLLAMA_URL.
// Empty means extractive — which is also what keeps tests hermetic.
func (c *Client) Backend() string {
	switch b := strings.ToLower(c.get("llm")); b {
	case "ollama", "claude", "openai":
		return b
	}
	if c.ollamaURL() != "" {
		return "ollama"
	}
	return ""
}

// Available reports whether generative features are on.
func (c *Client) Available() bool { return c.Backend() != "" }

func (c *Client) post(url string, headers map[string]string, body any) ([]byte, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// Complete runs one raw completion on the active backend. It returns an error
// when no backend or key is configured, so every caller can fall back.
func (c *Client) Complete(prompt, backend string) (string, error) {
	if backend == "" {
		backend = c.Backend()
	}
	switch backend {
	case "ollama":
		// think:false matters: a reasoning model otherwise spends seconds
		// emitting a chain of thought nobody reads here
		out, err := c.post(c.ollamaURL()+"/api/generate", nil, map[string]any{
			"model": c.model(), "prompt": prompt, "stream": false, "think": false})
		if err != nil {
			return "", err
		}
		var r struct {
			Response string `json:"response"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return "", err
		}
		return strings.TrimSpace(r.Response), nil

	case "openai":
		base := strings.TrimRight(c.get("llm_base_url"), "/")
		if base == "" {
			base = "https://api.openai.com/v1"
		}
		headers := map[string]string{}
		if k := c.apiKey(); k != "" {
			headers["Authorization"] = "Bearer " + k
		}
		out, err := c.post(base+"/chat/completions", headers, map[string]any{
			"model": c.model(), "stream": false, "temperature": 0.2,
			"messages": []map[string]string{{"role": "user", "content": prompt}}})
		if err != nil {
			return "", err
		}
		var r struct {
			Choices []struct {
				Message struct {
					Content string `json:"content"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return "", err
		}
		if len(r.Choices) == 0 {
			return "", fmt.Errorf("no choices returned")
		}
		return strings.TrimSpace(r.Choices[0].Message.Content), nil

	case "claude":
		key := os.Getenv("ANTHROPIC_API_KEY")
		if key == "" {
			key = c.apiKey()
		}
		if key == "" {
			return "", fmt.Errorf("no anthropic key")
		}
		model := c.model()
		if model == "qwen3.5:4b" { // the Ollama default is meaningless here
			model = "claude-sonnet-5"
		}
		out, err := c.post("https://api.anthropic.com/v1/messages", map[string]string{
			"x-api-key": key, "anthropic-version": "2023-06-01"}, map[string]any{
			"model": model, "max_tokens": 1024,
			"messages": []map[string]string{{"role": "user", "content": prompt}}})
		if err != nil {
			return "", err
		}
		var r struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(out, &r); err != nil {
			return "", err
		}
		var b strings.Builder
		for _, part := range r.Content {
			b.WriteString(part.Text)
		}
		return strings.TrimSpace(b.String()), nil
	}
	return "", fmt.Errorf("no llm backend configured")
}

// Context is one retrieved passage offered to the answerer.
type Context struct {
	Path  string
	Title string
	Chunk string
}

// Answer synthesizes an answer from retrieved contexts: generative when an LLM
// is available, extractive otherwise. An LLM failure falls through to
// extractive rather than failing the request.
func (c *Client) Answer(question string, contexts []Context) string {
	if len(contexts) == 0 {
		return "I couldn't find anything in your notes about that."
	}
	if backend := c.Backend(); backend != "" {
		if out, err := c.llmAnswer(question, contexts, backend); err == nil && out != "" {
			return out
		}
	}
	return ExtractiveAnswer(question, contexts)
}

func (c *Client) llmAnswer(question string, contexts []Context, backend string) (string, error) {
	n := min(len(contexts), 6)
	parts := make([]string, n)
	for i, ctx := range contexts[:n] {
		parts[i] = fmt.Sprintf("[%d] (%s)\n%s", i+1, ctx.Title, ctx.Chunk)
	}
	prompt := "Answer the question using ONLY the notes below. Cite sources as [n]. " +
		"If the notes don't contain the answer, say so.\n\nNOTES:\n" +
		strings.Join(parts, "\n\n") + "\n\nQUESTION: " + question + "\n\nANSWER:"
	return c.Complete(prompt, backend)
}

// ExtractiveAnswer stitches the best passages together with wiki-link
// citations. This is the offline floor, and a deliberate one: a wrong
// generated answer about your own notes is worse than no answer, and the
// citations are what make it checkable.
func ExtractiveAnswer(question string, contexts []Context) string {
	var parts []string
	for _, ctx := range contexts[:min(len(contexts), 4)] {
		snippet := strings.TrimSpace(ctx.Chunk)
		if utf8.RuneCountInString(snippet) > 400 {
			cut := runeCut(snippet, 400)
			if i := strings.LastIndex(cut, " "); i > 0 {
				cut = cut[:i]
			}
			snippet = cut + " …"
		}
		parts = append(parts, fmt.Sprintf("- %s  ([[%s|%s]])", snippet, stem(ctx.Path), ctx.Title))
	}
	return fmt.Sprintf("From your notes on “%s”:\n\n%s",
		strings.TrimSpace(question), strings.Join(parts, "\n"))
}

// runeCut truncates to n CHARACTERS, not bytes — Python slices strings by
// character, and a byte cut would both differ and be able to split a rune.
func runeCut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// stem is the citation target for a path: its basename with the ".md" dropped.
// It drops the last three CHARACTERS rather than the suffix, matching
// server/ai.py's `path.rsplit("/", 1)[-1][:-3]` — which for a short synthetic
// path like "_" yields an empty stem, and the citation must match there too.
func stem(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		path = path[i+1:]
	}
	r := []rune(path)
	if len(r) <= 3 {
		return ""
	}
	return string(r[:len(r)-3])
}

var leadingBullet = regexp.MustCompile(`^[\d.)\-•*\s]+`)

// Decompose splits a complex question into the 1-3 focused sub-questions whose
// answers are needed to answer it. This is what lifts multi-hop retrieval — the
// one category where retrieval lagged full context on the benchmarks. Returns
// the question unchanged with no LLM, or when it is too short to be multi-hop.
func (c *Client) Decompose(question string) []string {
	q := strings.TrimSpace(question)
	backend := c.Backend()
	if backend == "" || len(strings.Fields(q)) < 6 {
		return []string{q}
	}
	out, err := c.Complete("Decompose the question into the 1-3 focused sub-questions whose "+
		"answers you would need to answer it. If it needs no decomposition, "+
		"return it unchanged. One per line, no numbering, no other text.\n\n"+
		"Question: "+q+"\n\nSub-questions:", backend)
	if err != nil {
		return []string{q}
	}
	var subs []string
	for _, ln := range strings.Split(out, "\n") {
		s := strings.TrimSpace(leadingBullet.ReplaceAllString(ln, ""))
		if len(s) > 4 {
			subs = append(subs, s)
		}
		if len(subs) == 3 {
			break
		}
	}
	if len(subs) == 0 {
		return []string{q}
	}
	return subs
}

var digits = regexp.MustCompile(`\d+`)

// Rerank orders candidate passages by how well each helps answer the question
// and returns the top `keep`. A no-op without an LLM or for a trivial pool.
// Candidates the LLM does not mention keep their original relative order at the
// end, so a partial or garbled reply degrades instead of dropping passages.
func (c *Client) Rerank(question string, candidates []Context, keep int) []Context {
	if keep < 0 {
		keep = 0
	}
	backend := c.Backend()
	if backend == "" || len(candidates) <= 1 {
		return candidates[:min(len(candidates), keep)]
	}
	listing := make([]string, 0, min(len(candidates), 20))
	for i, cand := range candidates[:min(len(candidates), 20)] {
		chunk := runeCut(cand.Chunk, 220)
		listing = append(listing, fmt.Sprintf("[%d] %s: %s", i, cand.Title, chunk))
	}
	out, err := c.Complete("Rank the passages by how well they help answer the question. "+
		"Reply with the most relevant passage numbers in order, "+
		"comma-separated (e.g. 3,1,5). Nothing else.\n\nQuestion: "+question+
		"\n\nPassages:\n"+strings.Join(listing, "\n")+"\n\nOrder:", backend)
	if err != nil {
		return candidates[:min(len(candidates), keep)]
	}
	seen := map[int]bool{}
	ranked := make([]Context, 0, len(candidates))
	for _, m := range digits.FindAllString(out, -1) {
		i, err := strconv.Atoi(m)
		if err != nil || i < 0 || i >= len(candidates) || seen[i] {
			continue
		}
		seen[i] = true
		ranked = append(ranked, candidates[i])
	}
	for i, cand := range candidates {
		if !seen[i] {
			ranked = append(ranked, cand)
		}
	}
	return ranked[:min(len(ranked), keep)]
}

// DedupLines drops duplicate memory entries — the zero-LLM consolidation
// floor.
//
// Duplicates are compared as FACTS, not as lines: two writes of the same
// belief differ in their trailer (each bullet carries its own id) and in
// capitalisation, so a line-equality test would call them distinct and leave
// the duplicate that consolidation exists to remove. A bullet that does not
// parse as an entry falls back to line equality, and a line that is not a
// bullet at all is never deduped — repeated prose is a person's writing.
func DedupLines(body string) string {
	seen := map[string]bool{}
	var out []string
	for _, ln := range strings.Split(body, "\n") {
		s := strings.TrimSpace(ln)
		if strings.HasPrefix(s, "- ") {
			key := s
			if e, ok := memory.ParseLine(ln); ok {
				key = e.Agent + "\x00" + memory.Normalize(e.Text)
			}
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		out = append(out, ln)
	}
	// Split on a trailing newline yields a final empty element; joining puts it
	// back, so only add one when the input had none to restore.
	joined := strings.Join(out, "\n")
	if strings.HasSuffix(body, "\n") && !strings.HasSuffix(joined, "\n") {
		joined += "\n"
	}
	return joined
}

// ConsolidateMemory rewrites a memory note so recall stays sharp as it grows:
// merge redundant entries, and when two conflict keep the most recent while
// noting the older is superseded — preserving each entry's provenance prefix.
// Falls back to exact-duplicate removal with no LLM. The caller snapshots
// first, so a bad rewrite is one rollback away.
func (c *Client) ConsolidateMemory(body string) string {
	backend := c.Backend()
	if backend == "" {
		return DedupLines(body)
	}
	out, err := c.Complete("Consolidate this agent memory note. Merge redundant entries; when two "+
		"entries conflict, keep the most recent and mark the older superseded; "+
		"keep every entry's provenance prefix (the **date · agent · task** "+
		"part); lose nothing important. Keep the format: a '# ' heading, then "+
		"attributed '- **…** — …' bullet entries. Return ONLY the rewritten "+
		"note.\n\nNOTE:\n"+body+"\n\nCONSOLIDATED:", backend)
	if err != nil {
		return DedupLines(body)
	}
	out = strings.TrimSpace(out)
	// guard: never let a degenerate rewrite destroy the memory
	if out == "" || len(out) < len(body)*3/10 {
		return DedupLines(body)
	}
	return out + "\n"
}

// Transcribe converts an audio memo to text using a local whisper HTTP service
// (GRIMOIRE_WHISPER_URL, OpenAI-compatible /v1/audio/transcriptions). Without
// one it returns a placeholder, so the memo is still saved with its audio
// attachment rather than the recording being lost to a missing service.
func (c *Client) Transcribe(audio []byte, filename string) string {
	url := strings.TrimRight(c.get("whisper_url"), "/")
	if url == "" {
		return "[audio memo — transcription unavailable; set GRIMOIRE_WHISPER_URL]"
	}
	if filename == "" {
		filename = "memo.webm"
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", filename)
	if err != nil {
		return "[audio memo — transcription failed]"
	}
	if _, err := part.Write(audio); err != nil {
		return "[audio memo — transcription failed]"
	}
	_ = mw.WriteField("model", "whisper-1")
	if err := mw.Close(); err != nil {
		return "[audio memo — transcription failed]"
	}
	req, err := http.NewRequest("POST", url+"/v1/audio/transcriptions", &buf)
	if err != nil {
		return "[audio memo — transcription failed]"
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "[audio memo — transcription failed]"
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode >= 400 {
		return "[audio memo — transcription failed]"
	}
	var r struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &r) != nil || strings.TrimSpace(r.Text) == "" {
		return "[audio memo — transcription failed]"
	}
	return strings.TrimSpace(r.Text)
}

// SuggestTags is the deterministic tag suggester: the most frequent
// content words. No LLM, so the editor button works offline.
func SuggestTags(text string) []string {
	stop := map[string]bool{"this": true, "that": true, "with": true, "from": true,
		"have": true, "your": true, "into": true, "notes": true, "which": true,
		"these": true, "there": true, "their": true, "about": true, "would": true,
		"could": true, "should": true}
	counts := map[string]int{}
	var order []string
	for _, w := range wordRe.FindAllString(strings.ToLower(text), -1) {
		if stop[w] {
			continue
		}
		if counts[w] == 0 {
			order = append(order, w)
		}
		counts[w]++
	}
	// most_common is stable on ties in first-seen order; sort.SliceStable over
	// first-seen order reproduces that
	sort.SliceStable(order, func(a, b int) bool { return counts[order[a]] > counts[order[b]] })
	return order[:min(len(order), 5)]
}

// wordRe matches Python's r"[a-z][a-z-]{3,}" — four characters minimum.
var wordRe = regexp.MustCompile(`[a-z][a-z-]{3,}`)

// FirstLineTitle is the deterministic title suggester.
func FirstLineTitle(text string) string {
	for _, line := range strings.Split(text, "\n") {
		s := strings.TrimSpace(strings.TrimLeft(line, "# "))
		if s != "" {
			return runeCut(s, 80)
		}
	}
	return "Untitled"
}
