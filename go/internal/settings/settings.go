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
	"llm":          {"GRIMOIRE_LLM", ""}, // '', 'ollama', 'claude', 'openai' ('' = auto)
	"llm_model":    {"GRIMOIRE_LLM_MODEL", "qwen3.5:4b"},
	"llm_base_url": {"GRIMOIRE_LLM_BASE_URL", ""},
	"llm_api_key":  {"GRIMOIRE_LLM_API_KEY", ""},
	// Reasoning effort for structured calls (memory-bank extraction and the
	// like): low|medium|high|max is passed through as reasoning_effort, and
	// "off" turns thinking off on servers that document a switch for it.
	"llm_reasoning_effort": {"GRIMOIRE_LLM_REASONING_EFFORT", ""},
	// A JSON object merged into every OpenAI-compatible request body, for the
	// vendor-specific fields no generic setting can anticipate.
	"llm_extra_body":    {"GRIMOIRE_LLM_EXTRA_BODY", ""},
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
	// Reranking retrieved passages before they are used; see
	// internal/rerank. rerank_model is a hub repo id or a local directory
	// for the local cross-encoder, or the model name a remote service
	// expects.
	"rerank":         {"GRIMOIRE_RERANK", "auto"}, // auto|local|remote|off
	"rerank_model":   {"GRIMOIRE_RERANK_MODEL", "cross-encoder/ms-marco-MiniLM-L-6-v2"},
	"rerank_url":     {"GRIMOIRE_RERANK_URL", ""},
	"rerank_api_key": {"GRIMOIRE_RERANK_API_KEY", ""},
	"rerank_max_len": {"GRIMOIRE_RERANK_MAX_LEN", "256"},
	// Memory-bank webhooks refuse loopback and private-network targets
	// unless this is on; link-local and cloud metadata stay refused.
	"webhook_allow_private": {"GRIMOIRE_WEBHOOK_ALLOW_PRIVATE", ""},
	// Background workers for memory-bank operations (async retain,
	// consolidation, mental-model refresh). 0 turns them off.
	"bank_workers": {"GRIMOIRE_BANK_WORKERS", "2"},
	// Dreaming: the periodic offline pass over agent memory (hygiene and a
	// security sweep). Hours between dreams; 0 turns the schedule off, and a
	// dream still only runs when memory changed since the last one.
	"dream_interval_hours": {"GRIMOIRE_DREAM_INTERVAL_HOURS", "24"},
	// What a scheduled dream may change: "safe" applies mechanical, reversible
	// fixes (index repair) and queues due bank consolidations; "off" only
	// reports.
	"dream_apply": {"GRIMOIRE_DREAM_APPLY", "safe"},
	// Vault folder the dream report note is written to.
	"dream_report_dir": {"GRIMOIRE_DREAM_REPORT_DIR", "Dreams"},
	// Agent memory freshness: the probability a fact has changed since it was
	// last verified above which recall tells the agent to re-check it before
	// use. Lower re-checks more (fresher, more lookups); higher re-checks less.
	"memory_verify_threshold": {"GRIMOIRE_MEMORY_VERIFY_THRESHOLD", "0.3"},
	// Optional typed-decision model, spoken over TypeSafe's Jev wire format
	// (POST /v1/systemone): Jev itself (https://api.typesafe.ai, paid, key
	// required) or a local laya-serve (http://127.0.0.1:8000, free). Empty
	// is off. When set, a fact remembered without a freshness tier is asked
	// "does this describe state that changes?" and the probability sets its
	// change-rate prior. The key may also live in the credential vault as
	// 'decision-api-key'. See docs/FRESHNESS.md for when this helps.
	"decision_url":     {"GRIMOIRE_DECISION_URL", ""},
	"decision_model":   {"GRIMOIRE_DECISION_MODEL", "jev-latest"},
	"decision_api_key": {"GRIMOIRE_DECISION_API_KEY", ""},
	// Platt calibration "a,b" applied to the volatility probability:
	// sigmoid(a*logit(p)+b). Raw jev-1.13.0 probabilities run high (stable
	// facts centre near 0.45); the default was fitted on 90 hand-labelled
	// facts from a real store and held up leave-one-out (log loss 0.60 raw,
	// 0.36 calibrated). "1,0" turns calibration off.
	"decision_calibration": {"GRIMOIRE_DECISION_CALIBRATION", "1.837,-2.493"},
	// Re-tell judge: a typed-decision model asked whether a prompt restates a
	// memory on file, so the prompt before it can be learned as that memory's
	// cue (docs/MEMORY_USE.md). Off when retell_url is empty. The key falls back
	// to the vault's 'retell-api-key', then to the decision key.
	"retell_url":       {"GRIMOIRE_RETELL_URL", ""},
	"retell_model":     {"GRIMOIRE_RETELL_MODEL", "jev-latest"},
	"retell_api_key":   {"GRIMOIRE_RETELL_API_KEY", ""},
	"retell_threshold": {"GRIMOIRE_RETELL_THRESHOLD", "0.9"},
	// Injection gate: a typed-decision model asked, for candidates whose
	// relevance falls inside the band, whether the memory applies to the
	// request. 400 ms hard budget; on timeout the score rule stands. Off when
	// context_gate_url is empty (docs/MEMORY_ADHERENCE.md). The key falls back
	// to the decision key.
	"context_gate_url":   {"GRIMOIRE_CONTEXT_GATE_URL", ""},
	"context_gate_model": {"GRIMOIRE_CONTEXT_GATE_MODEL", "jev-latest"},
	"context_gate_band":  {"GRIMOIRE_CONTEXT_GATE_BAND", "0.5,0.75"},
	// Most candidates one request sends to the gate, and the yes-probability a
	// memory needs to be kept. A decision server that answers one question at a
	// time (local laya-serve, ~170 ms each) fits two inside the 400 ms budget.
	"context_gate_max":       {"GRIMOIRE_CONTEXT_GATE_MAX", "4"},
	"context_gate_threshold": {"GRIMOIRE_CONTEXT_GATE_THRESHOLD", "0.5"},
	// Optional: an agent's per-project directory (e.g. ~/.claude/projects) and
	// the memory directory every project is meant to share. When both are
	// set, a dream reports projects whose memory/ is a separate real
	// directory, since what is written there is invisible everywhere else.
	"dream_projects_dir":     {"GRIMOIRE_DREAM_PROJECTS_DIR", ""},
	"dream_canonical_memory": {"GRIMOIRE_DREAM_CANONICAL_MEMORY", ""},
	// The shared memory store (docs/MEMORY_STORE.md): the directory every
	// agent's memory location is linked to. Falls back to
	// dream_canonical_memory when empty.
	"memory_canonical_dir": {"GRIMOIRE_MEMORY_CANONICAL_DIR", ""},
	// Procedure verification: ports a procedure check may probe (comma
	// separated; empty probes none) and how many due procedures one dream
	// checks.
	"memory_verify_ports":     {"GRIMOIRE_MEMORY_VERIFY_PORTS", ""},
	"memory_other_hosts":      {"GRIMOIRE_MEMORY_OTHER_HOSTS", ""},
	"memory_verify_per_dream": {"GRIMOIRE_MEMORY_VERIFY_PER_DREAM", "3"},
}

// InternalFields are persisted in the same file and resolved the same way, but
// are owned by a dedicated surface rather than the generic settings form: the
// cloud folder sync is configured through a flow that also derives a key, and
// a bare text box that changed the folder would bypass it.
var InternalFields = map[string]Field{
	"sync_folder":          {"GRIMOIRE_SYNC_FOLDER", ""},
	"sync_folder_interval": {"GRIMOIRE_SYNC_FOLDER_INTERVAL", "60"},
	"device_name":          {"GRIMOIRE_DEVICE_NAME", ""},
}

func lookup(key string) (Field, bool) {
	if f, ok := Fields[key]; ok {
		return f, true
	}
	f, ok := InternalFields[key]
	return f, ok
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
	f, known := lookup(key)
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
	return s.update(patch, Fields)
}

// UpdateInternal is Update for InternalFields.
func (s *Store) UpdateInternal(patch map[string]string) error {
	return s.update(patch, InternalFields)
}

func (s *Store) update(patch map[string]string, allowed map[string]Field) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	raw, _ := os.ReadFile(s.path)
	cur := map[string]string{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cur)
	}
	for k, v := range patch {
		if _, known := allowed[k]; !known {
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
