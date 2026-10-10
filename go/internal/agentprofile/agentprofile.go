// Package agentprofile describes a coding agent in one JSON file: where its
// hooks and MCP entry live, what its hook events are called, how a tool call
// is shaped, and how it wants context handed back. Memory injection and
// `grimoire agent install` are driven from it, so supporting a new agent is
// writing a profile, not code.
//
// Built-ins are embedded (builtin/*.json). A file in
// ~/.config/grimoire/agents/NAME.json overrides the built-in of that name or
// adds a new agent; it is decoded over the built-in it replaces (or over
// `generic`), so it only needs the fields that differ. Resolve writes the
// result to ~/.grimoire/agents/NAME.json, which is what the Python hook reads.
package agentprofile

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

//go:embed builtin/*.json
var builtinFS embed.FS

// ContextScript is the hook that injects memory, shipped inside the binary.
// clients/hooks/grimoire_context.py is the file people read and test;
// TestEmbeddedContextHookMatchesTheClientCopy keeps the two identical.
//
//go:embed grimoire_context.py
var ContextScript []byte

// ContextFileName is what the installer writes it as, and how it recognises
// its own hook entries.
const ContextFileName = "grimoire_context.py"

// OutcomeScript is the adherence hook (post-action and stop events), shipped
// inside the binary; clients/hooks/grimoire_outcome.py is the copy people read
// and TestEmbeddedOutcomeHookMatchesTheClientCopy keeps them identical.
//
//go:embed grimoire_outcome.py
var OutcomeScript []byte

// OutcomeFileName is what the installer writes it as.
const OutcomeFileName = "grimoire_outcome.py"

// Events are the logical events. A profile maps each to the agent's own name
// for it; an event the agent lacks is simply absent.
var Events = []string{"session_start", "prompt", "pre_action", "post_action", "post_action_failure", "stop", "session_end", "file_read"}

// Outputs are the ways a hook can hand context back.
//
//	claude-json   {"hookSpecificOutput":{"hookEventName":..,"additionalContext":..}} (Claude Code, Codex)
//	plain-stdout  the text itself, for agents that append a hook's stdout
var Outputs = []string{"claude-json", "plain-stdout"}

type Hooks struct {
	File     string            `json:"file"`
	Format   string            `json:"format"` // claude-json: {"hooks":{EVENT:[{matcher,hooks:[...]}]}}
	Trust    string            `json:"trust,omitempty"`
	Events   map[string]string `json:"events"`
	Matchers map[string]string `json:"matchers,omitempty"`
	Timeouts map[string]int    `json:"timeouts,omitempty"`
}

type MCP struct {
	File   string `json:"file"`
	Format string `json:"format"` // json (mcpServers) | toml (mcp_servers)
}

type Memory struct {
	Files []string `json:"files,omitempty"` // instruction files the agent reads (managed block)
	Dir   string   `json:"dir,omitempty"`   // directory-shaped memory
	// Glob lists patterns whose matches are directory-shaped memory (Claude
	// Code keeps one memory/ per project: ~/.claude/projects/*/memory).
	Glob []string `json:"glob,omitempty"`
}

// TranscriptFormats are the formats the transcript package can read. A test in
// that package keeps this list and its readers in step.
var TranscriptFormats = []string{"claude-jsonl", "codex-rollout", "opencode", "pi", "cursor", "generic-jsonl"}

// Transcripts says where an agent's session transcripts are and how to read
// them, so memory can learn from sessions of any agent.
type Transcripts struct {
	// Glob is a path pattern (~ expanded; * matches within one path segment,
	// ** across segments). For opencode and cursor it names the database file.
	Glob   string `json:"glob,omitempty"`
	Format string `json:"format,omitempty"`
	// Map is for generic-jsonl: where each normalised field lives in a line.
	// Keys: session, cwd, time, model, role, text, tool, tool_target,
	// tool_error. Values are dotted JSON paths ("a|b" tries a then b).
	Map map[string]string `json:"map,omitempty"`
}

type Profile struct {
	Name            string            `json:"name"`
	Description     string            `json:"description,omitempty"`
	Detect          []string          `json:"detect"`
	Hooks           Hooks             `json:"hooks"`
	EventFields     map[string]string `json:"event_fields"`
	Actions         map[string]string `json:"actions"`
	DelegationTools []string          `json:"delegation_tools"`
	SessionFallback bool              `json:"session_fallback,omitempty"`
	Output          string            `json:"output"`
	MCP             MCP               `json:"mcp"`
	Memory          Memory            `json:"memory"`
	Transcripts     Transcripts       `json:"transcripts"`
	SkillsDir       string            `json:"skills_dir,omitempty"` // where the agent loads native skills from
	Source          string            `json:"source,omitempty"`     // set on load: builtin | user
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// ValidName reports whether s can name a profile (it becomes a file name).
func ValidName(s string) bool { return nameRE.MatchString(s) }

func builtin(name string) (*Profile, error) {
	raw, err := builtinFS.ReadFile("builtin/" + name + ".json")
	if err != nil {
		return nil, fmt.Errorf("no built-in profile %q", name)
	}
	p := &Profile{}
	if err := json.Unmarshal(raw, p); err != nil {
		return nil, fmt.Errorf("built-in %s: %w", name, err)
	}
	p.Source = "builtin"
	return p, nil
}

// BuiltinNames lists the embedded profiles.
func BuiltinNames() []string {
	entries, _ := fs.ReadDir(builtinFS, "builtin")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}

// UserDir is where a person's own profiles live.
func UserDir(home string) string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "grimoire", "agents")
	}
	return filepath.Join(home, ".config", "grimoire", "agents")
}

// ResolvedDir is where Resolve writes the copy the hook reads.
func ResolvedDir(home string) string { return filepath.Join(home, ".grimoire", "agents") }

// Load returns the named profile with user overrides applied and ~ expanded.
func Load(name, home string) (*Profile, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid agent name %q (lowercase letters, digits, - and _, starting with a letter)", name)
	}
	file := filepath.Join(UserDir(home), name+".json")
	base, berr := builtin(name)
	raw, err := os.ReadFile(file)
	switch {
	case err == nil:
		if berr != nil { // a new agent starts from generic
			if base, berr = builtin("generic"); berr != nil {
				return nil, berr
			}
		}
		base.Source = "user"
		if err := overlay(base, raw); err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
	case errors.Is(err, os.ErrNotExist):
		if berr != nil {
			return nil, fmt.Errorf("unknown agent %q (see `grimoire agent profiles`)", name)
		}
	default:
		return nil, err
	}
	base.Name = name
	base.expand(home)
	if err := base.Validate(); err != nil {
		return nil, fmt.Errorf("profile %s: %w", name, err)
	}
	return base, nil
}

// overlay applies a user's file over p. A field the file names replaces that
// field whole (inside hooks, mcp and memory, per sub-field), so a new agent's
// events are exactly the ones it lists and not generic's as well. Unknown
// fields are an error, so a typo is not silently ignored.
func overlay(p *Profile, raw []byte) error {
	var fresh Profile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fresh); err != nil {
		return err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return err
	}
	var sub struct {
		Hooks, MCP, Memory, Transcripts map[string]json.RawMessage
	}
	_ = json.Unmarshal([]byte(`{"Hooks":`+orEmpty(top["hooks"])+`,"MCP":`+orEmpty(top["mcp"])+`,"Memory":`+orEmpty(top["memory"])+`,"Transcripts":`+orEmpty(top["transcripts"])+`}`), &sub)
	has := func(k string) bool { _, ok := top[k]; return ok }
	if has("description") {
		p.Description = fresh.Description
	}
	if has("detect") {
		p.Detect = fresh.Detect
	}
	if has("event_fields") {
		p.EventFields = fresh.EventFields
	}
	if has("actions") {
		p.Actions = fresh.Actions
	}
	if has("delegation_tools") {
		p.DelegationTools = fresh.DelegationTools
	}
	if has("session_fallback") {
		p.SessionFallback = fresh.SessionFallback
	}
	if has("output") {
		p.Output = fresh.Output
	}
	for k := range sub.Hooks {
		switch k {
		case "file":
			p.Hooks.File = fresh.Hooks.File
		case "format":
			p.Hooks.Format = fresh.Hooks.Format
		case "trust":
			p.Hooks.Trust = fresh.Hooks.Trust
		case "events":
			p.Hooks.Events = fresh.Hooks.Events
		case "matchers":
			p.Hooks.Matchers = fresh.Hooks.Matchers
		case "timeouts":
			p.Hooks.Timeouts = fresh.Hooks.Timeouts
		}
	}
	for k := range sub.MCP {
		switch k {
		case "file":
			p.MCP.File = fresh.MCP.File
		case "format":
			p.MCP.Format = fresh.MCP.Format
		}
	}
	for k := range sub.Memory {
		switch k {
		case "files":
			p.Memory.Files = fresh.Memory.Files
		case "dir":
			p.Memory.Dir = fresh.Memory.Dir
		case "glob":
			p.Memory.Glob = fresh.Memory.Glob
		}
	}
	for k := range sub.Transcripts {
		switch k {
		case "glob":
			p.Transcripts.Glob = fresh.Transcripts.Glob
		case "format":
			p.Transcripts.Format = fresh.Transcripts.Format
		case "map":
			p.Transcripts.Map = fresh.Transcripts.Map
		}
	}
	if has("skills_dir") {
		p.SkillsDir = fresh.SkillsDir
	}
	return nil
}

func orEmpty(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	return string(raw)
}

// All loads every built-in and every user profile, sorted by name. A user file
// that fails to load is reported in errs and skipped, not fatal.
func All(home string) (profiles []*Profile, errs []error) {
	names := map[string]bool{}
	for _, n := range BuiltinNames() {
		names[n] = true
	}
	if entries, err := os.ReadDir(UserDir(home)); err == nil {
		for _, e := range entries {
			if n := strings.TrimSuffix(e.Name(), ".json"); n != e.Name() && !e.IsDir() && ValidName(n) {
				names[n] = true
			}
		}
	}
	var sorted []string
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	for _, n := range sorted {
		p, err := Load(n, home)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		profiles = append(profiles, p)
	}
	return profiles, errs
}

func expandPath(s, home string) string {
	if s == "~" {
		return home
	}
	if strings.HasPrefix(s, "~/") {
		return filepath.Join(home, s[2:])
	}
	return s
}

func (p *Profile) expand(home string) {
	p.Hooks.File = expandPath(p.Hooks.File, home)
	p.MCP.File = expandPath(p.MCP.File, home)
	p.Memory.Dir = expandPath(p.Memory.Dir, home)
	p.Transcripts.Glob = expandPath(p.Transcripts.Glob, home)
	p.SkillsDir = expandPath(p.SkillsDir, home)
	for i, g := range p.Memory.Glob {
		p.Memory.Glob[i] = expandPath(g, home)
	}
	for i, d := range p.Detect {
		p.Detect[i] = expandPath(d, home)
	}
	for i, f := range p.Memory.Files {
		p.Memory.Files[i] = expandPath(f, home)
	}
}

// Detected reports whether the agent seems to be installed here.
func (p *Profile) Detected() bool {
	for _, d := range p.Detect {
		if _, err := os.Stat(d); err == nil {
			return true
		}
	}
	return false
}

// Dir is the agent's config folder: where its hooks file lives.
func (p *Profile) Dir() string {
	if p.Hooks.File == "" {
		return ""
	}
	return filepath.Dir(p.Hooks.File)
}

func knownEvent(e string) bool {
	for _, k := range Events {
		if k == e {
			return true
		}
	}
	return false
}

// Validate checks a profile is usable. It is strict about the things that end
// up in a command line or a file path.
func (p *Profile) Validate() error {
	if !ValidName(p.Name) {
		return fmt.Errorf("invalid name %q", p.Name)
	}
	if !contains(Outputs, p.Output) {
		return fmt.Errorf("output %q must be one of %s", p.Output, strings.Join(Outputs, ", "))
	}
	if p.Hooks.Format != "" && p.Hooks.Format != "claude-json" {
		return fmt.Errorf("hooks.format %q is not supported (claude-json)", p.Hooks.Format)
	}
	if p.Hooks.Trust != "" && p.Hooks.Trust != "codex" {
		return fmt.Errorf("hooks.trust %q is not supported (codex)", p.Hooks.Trust)
	}
	for logical, native := range p.Hooks.Events {
		if !knownEvent(logical) {
			return fmt.Errorf("hooks.events: unknown event %q (known: %s)", logical, strings.Join(Events, ", "))
		}
		if strings.TrimSpace(native) == "" || strings.ContainsAny(native, " \t\n'\"") {
			return fmt.Errorf("hooks.events.%s: %q is not a usable event name", logical, native)
		}
	}
	for logical := range p.Hooks.Matchers {
		if !knownEvent(logical) {
			return fmt.Errorf("hooks.matchers: unknown event %q", logical)
		}
	}
	for logical, t := range p.Hooks.Timeouts {
		if !knownEvent(logical) || t < 1 || t > 600 {
			return fmt.Errorf("hooks.timeouts.%s: %d (a known event, 1-600 seconds)", logical, t)
		}
	}
	if p.Hooks.File != "" && !filepath.IsAbs(p.Hooks.File) {
		return fmt.Errorf("hooks.file %q must be absolute or start with ~/", p.Hooks.File)
	}
	if p.MCP.Format != "" && p.MCP.Format != "json" && p.MCP.Format != "toml" {
		return fmt.Errorf("mcp.format %q must be json or toml", p.MCP.Format)
	}
	if (p.MCP.File == "") != (p.MCP.Format == "") {
		return errors.New("mcp.file and mcp.format go together")
	}
	if p.MCP.File != "" && !filepath.IsAbs(p.MCP.File) {
		return fmt.Errorf("mcp.file %q must be absolute or start with ~/", p.MCP.File)
	}
	if p.Hooks.Events["prompt"] != "" && p.EventFields["prompt"] == "" {
		return errors.New("event_fields.prompt is required when the prompt event is set")
	}
	if p.Hooks.Events["pre_action"] != "" && p.EventFields["tool"] == "" {
		return errors.New("event_fields.tool is required when the pre_action event is set")
	}
	if p.Transcripts.Format != "" && !contains(TranscriptFormats, p.Transcripts.Format) {
		return fmt.Errorf("transcripts.format %q must be one of %s", p.Transcripts.Format, strings.Join(TranscriptFormats, ", "))
	}
	if (p.Transcripts.Glob == "") != (p.Transcripts.Format == "") {
		return errors.New("transcripts.glob and transcripts.format go together")
	}
	if p.Transcripts.Glob != "" && !filepath.IsAbs(p.Transcripts.Glob) {
		return fmt.Errorf("transcripts.glob %q must be absolute or start with ~/", p.Transcripts.Glob)
	}
	if p.Transcripts.Format == "generic-jsonl" && p.Transcripts.Map["role"] == "" && p.Transcripts.Map["text"] == "" && p.Transcripts.Map["tool"] == "" {
		return errors.New("transcripts.map needs at least role and text (or tool) for generic-jsonl")
	}
	if p.SkillsDir != "" && !filepath.IsAbs(p.SkillsDir) {
		return fmt.Errorf("skills_dir %q must be absolute or start with ~/", p.SkillsDir)
	}
	for tool, field := range p.Actions {
		if tool == "" || field == "" {
			return errors.New("actions: tool and field names must be non-empty")
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Resolve writes the profile as the hook reads it, to dir/NAME.json, and
// reports whether the file changed.
func (p *Profile) Resolve(dir string, dryRun bool) (path string, changed bool, err error) {
	path = filepath.Join(dir, p.Name+".json")
	next, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return path, false, err
	}
	next = append(next, '\n')
	if cur, rerr := os.ReadFile(path); rerr == nil && string(cur) == string(next) {
		return path, false, nil
	}
	if dryRun {
		return path, true, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return path, true, err
	}
	tmp, err := os.CreateTemp(dir, ".grimoire-tmp-*")
	if err != nil {
		return path, true, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(next); err != nil {
		tmp.Close()
		return path, true, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return path, true, err
	}
	if err := tmp.Close(); err != nil {
		return path, true, err
	}
	return path, true, os.Rename(tmp.Name(), path)
}

// Starter returns the text of a new user profile for name, copied from
// generic: complete, so it can be edited in place without reading the docs.
func Starter(name string) ([]byte, error) {
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid agent name %q", name)
	}
	g, err := builtin("generic")
	if err != nil {
		return nil, err
	}
	g.Name = name
	g.Source = ""
	g.Description = "Describe " + name + " here"
	out, err := json.MarshalIndent(g, "", "  ")
	return append(out, '\n'), err
}

// MemoryDirs returns the directory-shaped memory locations that exist now:
// memory.dir plus every match of memory.glob.
func (p *Profile) MemoryDirs() []string {
	seen := map[string]bool{}
	var out []string
	add := func(d string) {
		if d == "" || seen[d] {
			return
		}
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			seen[d] = true
			out = append(out, d)
		}
	}
	add(p.Memory.Dir)
	for _, g := range p.Memory.Glob {
		matches, _ := filepath.Glob(g)
		sort.Strings(matches)
		for _, m := range matches {
			add(m)
		}
	}
	return out
}
