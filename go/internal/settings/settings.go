// Package settings is a small JSON store in .grimoire/settings.json.
//
// Port of server/settings.py. These override environment defaults so the AI
// backend and model can be changed from the console without editing a systemd
// unit. Only non-secret operational settings live here.
//
// Precedence for a value: settings.json → environment → built-in default.
package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// Field is a settable option and where its fallback comes from.
type Field struct {
	EnvKey  string
	Default string
}

// Fields are the settings that may be set from the UI.
//
// embed_model is deliberately present but NOT editable through the API:
// changing it would invalidate every stored vector.
var Fields = map[string]Field{
	"llm":               {"GRIMOIRE_LLM", ""}, // '', 'ollama', 'claude', 'openai' ('' = auto)
	"llm_model":         {"GRIMOIRE_LLM_MODEL", "qwen3.5:4b"},
	"llm_base_url":      {"GRIMOIRE_LLM_BASE_URL", ""},
	"llm_api_key":       {"GRIMOIRE_LLM_API_KEY", ""},
	"ollama_url":        {"GRIMOIRE_OLLAMA_URL", ""},
	"embed_model":       {"GRIMOIRE_EMBED_MODEL", "nomic-embed-text"},
	"embed_base_url":    {"GRIMOIRE_EMBED_BASE_URL", ""},
	"embed_api_key":     {"GRIMOIRE_EMBED_API_KEY", ""},
	"local_embed":       {"GRIMOIRE_LOCAL_EMBED", "auto"},
	"local_embed_model": {"GRIMOIRE_LOCAL_EMBED_MODEL", "minishlab/potion-base-8M"},
	"whisper_url":       {"GRIMOIRE_WHISPER_URL", ""},
	// The public published site, off unless an operator turns it on. A
	// surface with no principal behind it must not appear because somebody
	// typed a frontmatter key; see internal/api/publish.go.
	"publish": {"GRIMOIRE_PUBLISH", ""},
	// Agent memory. Both are prompt PREFIXES, not whole prompts: the output
	// contract the server parses is appended after whatever is set here, so a
	// deployment can bias extraction ("only record facts about
	// infrastructure") without being able to break the reply format the
	// engine depends on. See internal/ai/memory.go.
	"memory_extract_prompt": {"GRIMOIRE_MEMORY_EXTRACT_PROMPT", ""},
	"memory_decide_prompt":  {"GRIMOIRE_MEMORY_DECIDE_PROMPT", ""},
	// Web search. The key may name a vault credential ("vault:brave-key")
	// rather than being one, so a search key does not have to sit in a
	// settings file that gets copied around.
	"web_search_provider": {"GRIMOIRE_WEB_SEARCH_PROVIDER", ""}, // searxng|brave|serper|google
	"web_search_url":      {"GRIMOIRE_WEB_SEARCH_URL", ""},      // searxng only
	"web_search_key":      {"GRIMOIRE_WEB_SEARCH_KEY", ""},
	"web_search_cx":       {"GRIMOIRE_WEB_SEARCH_CX", ""}, // google programmable search id
}

// Store reads and writes the settings file.
type Store struct {
	path string
	mu   sync.RWMutex
}

func New(grimoireDir string) *Store {
	return &Store{path: filepath.Join(grimoireDir, "settings.json")}
}

func (s *Store) load() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return map[string]string{}
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return map[string]string{} // a corrupt file must not break startup
	}
	return m
}

// Get returns the effective value: settings.json wins, then env, then default.
func (s *Store) Get(key string) string {
	f, known := Fields[key]
	if v := s.load()[key]; v != "" {
		return v
	}
	if known && f.EnvKey != "" {
		if v := os.Getenv(f.EnvKey); v != "" {
			return v
		}
	}
	return f.Default
}

// AllEffective returns every known setting with its effective value.
func (s *Store) AllEffective() map[string]string {
	out := make(map[string]string, len(Fields))
	for k := range Fields {
		out[k] = s.Get(k)
	}
	return out
}

// Keys returns the field names in a stable order.
func Keys() []string {
	out := make([]string, 0, len(Fields))
	for k := range Fields {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Update merges a patch and persists it. Empty values clear a stored override
// so the environment default takes over again.
func (s *Store) Update(patch map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, _ := os.ReadFile(s.path)
	cur := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cur)
	}
	for k, v := range patch {
		if _, known := Fields[k]; !known {
			continue // never persist an unknown key from a request body
		}
		if v == "" {
			delete(cur, k)
			continue
		}
		cur[k] = v
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cur, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
