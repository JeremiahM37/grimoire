package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/agenthook"
	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
	"github.com/JeremiahM37/grimoire/go/internal/bank"
)

// grimoire agent install | uninstall | status
//
// One command that wires a coding agent to Grimoire: the session hooks and the
// MCP server entry, merged into the agent's own settings. It is meant to be
// run twice without harm. Anything it did not write is left exactly as it was,
// every file it changes is copied aside first, and `uninstall` removes only
// what `install` added.

const agentUsage = `grimoire agent — wire a coding agent to Grimoire in one command

  grimoire agent install [--claude-code] [--codex] [--agent NAME]... [--bank NAME] [--url URL]
                         [--recall] [--files] [--memory] [--no-mcp] [--no-tools] [--dry-run]
  grimoire agent uninstall [--claude-code] [--codex] [--agent NAME]... [--dry-run]
  grimoire agent status [--claude-code] [--codex] [--agent NAME]...
  grimoire agent profiles             list the agent profiles (built-in and yours)
  grimoire agent new NAME             write a starter profile to ~/.config/grimoire/agents/NAME.json

An agent is a profile (a JSON file): --claude-code and --codex are --agent claude-code
and --agent codex. With no agent given, those whose config folder exists (~/.claude,
~/.codex, or what a profile's "detect" names) are used. install merges, never replaces:
your other hooks and servers are untouched, a changed file is first copied to
FILE.grimoire-backup-TIME, and running it again changes nothing. --dry-run prints what
would change and writes nothing. No token is written anywhere; the agent's environment
supplies it.

Hooks: SessionStart (inject the bank's rules, knowledge and the last session's
digest), PostToolUse (local activity buffer), Stop and SessionEnd (retain the
session and write its digest). --recall adds UserPromptSubmit recall. --files (Claude
Code only, off by default) adds a PreToolUse hook on Read that injects what the bank
remembers about the file being read. --memory adds the memory hook: the memories that
bear on each prompt and on each command or edit as it happens (clients/hooks/grimoire_context.py,
docs/MEMORY_USE.md); it reads the profile written to ~/.grimoire/agents/NAME.json.
Codex runs a hook only after it is trusted, so install asks the installed codex for each
hook's hash and records it in a managed block of config.toml (uninstall removes it).
Files: Claude Code ~/.claude/settings.json and ~/.claude.json; Codex ~/.codex/hooks.json
and ~/.codex/config.toml. See docs/AGENTS_ANY.md to add another agent.`

// agentMarker identifies entries this command wrote.
const (
	agentMarker   = agenthook.FileName
	contextMarker = agentprofile.ContextFileName
	tomlBegin     = "# >>> grimoire agent install (managed block; `grimoire agent uninstall` removes it) >>>"
	tomlEnd       = "# <<< grimoire agent install <<<"
	trustBegin    = "# >>> grimoire agent install: hook trust (managed block; `grimoire agent uninstall` removes it) >>>"
	trustEnd      = "# <<< grimoire agent install: hook trust <<<"
	installedKey  = "GRIMOIRE_INSTALLED_BY"
	installedVal  = "grimoire agent install"
)

// agentTarget is a profile with its paths resolved: what the installer works on.
type agentTarget struct {
	name        string // profile name
	p           *agentprofile.Profile
	dir         string // config folder
	hooksFile   string
	mcpFile     string
	mcpIsTOML   bool
	hookHarness string
}

type hookSpec struct {
	event, matcher string
	timeout        int
	command        string // set by the caller that knows the script and its environment
	marker         string // which of our scripts the entry runs (agentMarker | contextMarker)
}

func newTarget(p *agentprofile.Profile) *agentTarget {
	return &agentTarget{name: p.Name, p: p, dir: p.Dir(), hooksFile: p.Hooks.File,
		mcpFile: p.MCP.File, mcpIsTOML: p.MCP.Format == "toml", hookHarness: p.Name}
}

// bankSpecs are the session-memory hooks, from the profile's events. Matchers and
// timeouts come from the profile too; the defaults are the ones Claude Code uses.
func bankSpecs(p *agentprofile.Profile, command string, recall, tools, files bool) []hookSpec {
	timeout := func(logical string, def int) int {
		if t := p.Hooks.Timeouts[logical]; t > 0 {
			return t
		}
		return def
	}
	var specs []hookSpec
	add := func(logical string, def int) {
		if native := p.Hooks.Events[logical]; native != "" {
			specs = append(specs, hookSpec{event: native, matcher: p.Hooks.Matchers[logical],
				timeout: timeout(logical, def), command: command, marker: agentMarker})
		}
	}
	add("session_start", 8)
	if files {
		add("file_read", 3)
	}
	if tools {
		add("post_action", 5)
	}
	add("stop", 20)
	add("session_end", 60)
	if recall {
		add("prompt", 4)
	}
	return specs
}

// contextSpecs are the memory-injection hooks: session start (resets the dedupe
// state after compaction), each prompt, and each action about to run.
func contextSpecs(p *agentprofile.Profile, command string) []hookSpec {
	var specs []hookSpec
	for _, logical := range []string{"session_start", "prompt", "pre_action"} {
		native := p.Hooks.Events[logical]
		if native == "" {
			continue
		}
		matcher := ""
		switch logical {
		case "session_start":
			matcher = p.Hooks.Matchers[logical]
		case "pre_action":
			matcher = p.Hooks.Matchers[logical]
			if matcher == "" {
				var tools []string
				for tool := range p.Actions {
					tools = append(tools, tool)
				}
				sort.Strings(tools)
				matcher = strings.Join(tools, "|")
			}
		}
		specs = append(specs, hookSpec{event: native, matcher: matcher, timeout: 3, command: command, marker: contextMarker})
	}
	return specs
}

// selectTargets picks the profiles to act on: those named, else those detected.
func selectTargets(f *bankFlags, home string) ([]*agentTarget, error) {
	var names []string
	for _, n := range []string{"claude-code", "codex"} {
		if f.on["--"+n] {
			names = append(names, n)
		}
	}
	names = append(names, f.values["--agent"]...)
	var chosen []*agentTarget
	seen := map[string]bool{}
	if len(names) == 0 {
		profiles, errs := agentprofile.All(home)
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "skipping a profile: %v\n", e)
		}
		for _, p := range profiles {
			if p.Hooks.File != "" && p.Detected() {
				chosen = append(chosen, newTarget(p))
			}
		}
		return chosen, nil
	}
	for _, n := range names {
		if seen[n] {
			continue
		}
		seen[n] = true
		p, err := agentprofile.Load(n, home)
		if err != nil {
			return nil, err
		}
		if p.Hooks.File == "" {
			return nil, fmt.Errorf("profile %q has no hooks.file; set it in %s", n, filepath.Join(agentprofile.UserDir(home), n+".json"))
		}
		chosen = append(chosen, newTarget(p))
	}
	return chosen, nil
}

func cmdAgent(args []string) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Println(agentUsage)
		if len(args) == 0 {
			return 2
		}
		return 0
	}
	action := args[0]
	switch action {
	case "install", "uninstall", "status", "profiles", "new":
	default:
		return fail("unknown agent command %q\n\n%s", action, agentUsage)
	}
	f, err := parseBankFlags(args[1:])
	if err != nil {
		return fail("%v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return fail("cannot find your home folder: %v", err)
	}
	switch action {
	case "profiles":
		return listProfiles(home)
	case "new":
		return newProfile(f.pos, home)
	}
	chosen, err := selectTargets(f, home)
	if err != nil {
		return fail("%v", err)
	}
	if len(chosen) == 0 {
		return fail("no agent found: neither ~/.claude nor ~/.codex exists, and no profile's agent was detected. Pass --claude-code, --codex or --agent NAME.")
	}
	opts := agentOptions{
		bank: f.str("--bank", ""), url: f.str("--url", os.Getenv("GRIMOIRE_URL")),
		recall: f.on["--recall"], files: f.on["--files"], context: f.on["--memory"], noMCP: f.on["--no-mcp"], noTools: f.on["--no-tools"],
		dryRun: f.on["--dry-run"], home: home,
	}
	if opts.bank != "" && !bank.ValidID(opts.bank) {
		return fail("invalid bank id %q (lowercase letters, digits and . _ : -)", opts.bank)
	}
	code := 0
	for _, t := range chosen {
		var lines []string
		var err error
		switch action {
		case "install":
			lines, err = installAgent(t, opts)
		case "uninstall":
			lines, err = uninstallAgent(t, opts)
		default:
			lines, err = statusAgent(t)
		}
		fmt.Printf("%s\n", t.name)
		for _, l := range lines {
			fmt.Println("  " + l)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "  error: %v\n", err)
			code = 1
		}
	}
	if opts.dryRun {
		fmt.Println("dry run: nothing was written")
	}
	return code
}

type agentOptions struct {
	bank, url, home                                string
	recall, files, context, noMCP, noTools, dryRun bool
}

func agentQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_@%+=:,./-") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hookCommand is the line an agent runs: the hook script with its settings
// given as environment variables, so the agent's own settings stay the only
// place that decides what is on.
func hookCommand(t *agentTarget, o agentOptions, script string) string {
	env := []string{"GRIMOIRE_BANK_SESSIONS=1", "GRIMOIRE_BANK_CONTEXT=1"}
	if !o.noTools {
		env = append(env, "GRIMOIRE_BANK_TOOLS=1")
	}
	if o.recall {
		env = append(env, "GRIMOIRE_BANK_RECALL=1")
	}
	if o.files && t.p.Hooks.Events["file_read"] != "" {
		env = append(env, "GRIMOIRE_BANK_FILES=1")
	}
	env = append(env, "GRIMOIRE_BANK_HARNESS="+t.hookHarness)
	if o.bank != "" {
		env = append(env, "GRIMOIRE_BANK="+agentQuote(o.bank))
	}
	if o.url != "" {
		env = append(env, "GRIMOIRE_URL="+agentQuote(o.url))
	}
	return strings.Join(env, " ") + " python3 " + agentQuote(script)
}

// installScript writes an embedded hook script where the agent will run it.
func installScript(script string, content []byte, dryRun bool) (string, error) {
	if cur, err := os.ReadFile(script); err == nil && bytes.Equal(cur, content) {
		return "hook script: " + script + " (up to date)", nil
	}
	if !dryRun {
		if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
			return "hook script: " + script, err
		}
		if err := writeAtomic(script, content, 0o755); err != nil {
			return "hook script: " + script, err
		}
	}
	return "hook script: " + script + " (written)", nil
}

// contextCommand is the memory hook's command line. The profile name is how the
// script finds ~/.grimoire/agents/NAME.json.
func contextCommand(t *agentTarget, o agentOptions, script string) string {
	env := []string{"GRIMOIRE_CONTEXT_MODE=all"}
	if o.url != "" {
		env = append(env, "GRIMOIRE_URL="+agentQuote(o.url))
	}
	return strings.Join(env, " ") + " python3 " + agentQuote(script) + " --agent " + t.name
}

func installAgent(t *agentTarget, o agentOptions) ([]string, error) {
	var out []string
	script := filepath.Join(o.home, ".grimoire", "hooks", agenthook.FileName)
	msg, err := installScript(script, agenthook.Script, o.dryRun)
	out = append(out, msg)
	if err != nil {
		return out, err
	}

	command := hookCommand(t, o, script)
	specs := bankSpecs(t.p, command, o.recall, !o.noTools, o.files)
	if o.context {
		ctxScript := filepath.Join(o.home, ".grimoire", "hooks", agentprofile.ContextFileName)
		msg, err := installScript(ctxScript, agentprofile.ContextScript, o.dryRun)
		out = append(out, msg)
		if err != nil {
			return out, err
		}
		path, changed, err := t.p.Resolve(agentprofile.ResolvedDir(o.home), o.dryRun)
		out = append(out, "profile: "+path+map[bool]string{true: " (written)", false: " (up to date)"}[changed])
		if err != nil {
			return out, err
		}
		specs = append(specs, contextSpecs(t.p, contextCommand(t, o, ctxScript))...)
	}
	root, raw, err := readJSONObject(t.hooksFile)
	if err != nil {
		return out, err
	}
	changed := mergeHooks(root, specs)
	next, err := encodeJSON(root)
	if err != nil {
		return out, err
	}
	msg, err = applyFile(t.hooksFile, raw, next, changed, o.dryRun)
	out = append(out, "hooks: "+msg)
	if err != nil {
		return out, err
	}

	if t.p.Hooks.Trust == "codex" {
		msg, err := trustCodexHooks(t, o)
		out = append(out, "hook trust: "+msg)
		if err != nil {
			return out, err
		}
	}

	if o.noMCP || t.mcpFile == "" {
		return out, nil
	}
	mcp, err := mcpBinary()
	if err != nil {
		out = append(out, "mcp: skipped ("+err.Error()+")")
		return out, nil
	}
	env := map[string]string{installedKey: installedVal, "GRIMOIRE_AGENT_NAME": t.name}
	if o.url != "" {
		env["GRIMOIRE_URL"] = o.url
	}
	if o.bank != "" {
		env["GRIMOIRE_BANK"] = o.bank
	}
	if t.mcpIsTOML {
		msg, err = installTOMLMCP(t.mcpFile, mcp, env, o.dryRun)
	} else {
		msg, err = installJSONMCP(t.mcpFile, mcp, env, o.dryRun)
	}
	out = append(out, "mcp: "+msg)
	return out, err
}

func uninstallAgent(t *agentTarget, o agentOptions) ([]string, error) {
	var out []string
	root, raw, err := readJSONObject(t.hooksFile)
	if err != nil {
		return out, err
	}
	changed := removeHooks(root)
	next, err := encodeJSON(root)
	if err != nil {
		return out, err
	}
	msg, err := applyFile(t.hooksFile, raw, next, changed, o.dryRun)
	out = append(out, "hooks: "+msg)
	if err != nil {
		return out, err
	}
	if t.p.Hooks.Trust == "codex" {
		msg, err = uninstallTrust(t.mcpFile, o.dryRun)
		out = append(out, "hook trust: "+msg)
		if err != nil {
			return out, err
		}
	}
	if !o.dryRun {
		// The copy of the profile the memory hook reads; harmless to leave, tidy to remove.
		_ = os.Remove(filepath.Join(agentprofile.ResolvedDir(o.home), t.name+".json"))
	}
	if t.mcpFile == "" {
		return out, nil
	}
	if t.mcpIsTOML {
		msg, err = uninstallTOMLMCP(t.mcpFile, o.dryRun)
	} else {
		msg, err = uninstallJSONMCP(t.mcpFile, o.dryRun)
	}
	out = append(out, "mcp: "+msg)
	return out, err
}

func statusAgent(t *agentTarget) ([]string, error) {
	root, _, err := readJSONObject(t.hooksFile)
	if err != nil {
		return nil, err
	}
	var events, memory []string
	if hooks, ok := root["hooks"].(map[string]any); ok {
		for ev, groups := range hooks {
			if groupsHaveMarker(groups, agentMarker) {
				events = append(events, ev)
			}
			if groupsHaveMarker(groups, contextMarker) {
				memory = append(memory, ev)
			}
		}
	}
	sort.Strings(events)
	sort.Strings(memory)
	out := []string{"hooks: " + orNone(strings.Join(events, ", ")),
		"memory hook: " + orNone(strings.Join(memory, ", "))}
	mcp := "not installed"
	if t.mcpFile == "" {
		mcp = "none for this profile"
	} else if t.mcpIsTOML {
		if b, err := os.ReadFile(t.mcpFile); err == nil && strings.Contains(string(b), tomlBegin) {
			mcp = "installed"
		}
	} else if r, _, err := readJSONObject(t.mcpFile); err == nil {
		if servers, ok := r["mcpServers"].(map[string]any); ok && ownedMCP(servers["grimoire"]) {
			mcp = "installed"
		}
	}
	return append(out, "mcp: "+mcp), nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// ---- JSON files -------------------------------------------------------------

// readJSONObject reads an agent settings file. A missing file is an empty
// object; one that is not a JSON object is an error, never overwritten.
func readJSONObject(path string) (map[string]any, []byte, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, raw, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return nil, nil, fmt.Errorf("%s is not a JSON object; not touching it (%v)", path, err)
	}
	return root, raw, nil
}

func encodeJSON(root map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// applyFile writes next over path if it differs, after copying the old file
// aside, and returns a one-line description.
func applyFile(path string, old, next []byte, changed, dryRun bool) (string, error) {
	if !changed && old != nil {
		return path + " (no change)", nil
	}
	if !changed && old == nil {
		return path + " (nothing to do)", nil
	}
	if dryRun {
		return path + " (would change)", nil
	}
	msg := path + " (updated"
	if old != nil {
		backup := path + ".grimoire-backup-" + time.Now().UTC().Format("20060102-150405")
		// Two edits to one file in the same second share a name; the first copy
		// is the user's original, so it is never overwritten.
		if _, err := os.Stat(backup); err != nil {
			if err := writeAtomic(backup, old, 0o600); err != nil {
				return msg + ")", fmt.Errorf("backing up %s: %w", path, err)
			}
		}
		msg += ", old copy at " + backup
	}
	mode := os.FileMode(0o600)
	if st, err := os.Stat(path); err == nil {
		mode = st.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return msg + ")", err
	}
	if err := writeAtomic(path, next, mode); err != nil {
		return msg + ")", err
	}
	return msg + ")", nil
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".grimoire-tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// owned reports whether a command is one of our hooks: it runs a script of ours.
func owned(command string, markers ...string) bool {
	if len(markers) == 0 {
		markers = []string{agentMarker, contextMarker}
	}
	for _, m := range markers {
		if strings.Contains(command, m) {
			return true
		}
	}
	return false
}

func groupsHaveMarker(groups any, markers ...string) bool {
	list, _ := groups.([]any)
	for _, g := range list {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); owned(c, markers...) {
				return true
			}
		}
	}
	return false
}

// dropOwned removes our hook entries from one event's groups, and reports
// whether anything went.
func dropOwned(groups []any, markers ...string) ([]any, bool) {
	changed := false
	var kept []any
	for _, g := range groups {
		gm, ok := g.(map[string]any)
		if !ok {
			kept = append(kept, g)
			continue
		}
		hs, _ := gm["hooks"].([]any)
		var keep []any
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); owned(c, markers...) {
				changed = true
				continue
			}
			keep = append(keep, h)
		}
		if len(keep) == 0 && len(hs) > 0 {
			continue // the group held only our entries
		}
		if len(keep) != len(hs) {
			gm["hooks"] = keep
		}
		kept = append(kept, gm)
	}
	return kept, changed
}

// ownsGroup reports whether a hook group holds an entry of ours.
func ownsGroup(g any, markers ...string) bool { return groupsHaveMarker([]any{g}, markers...) }

// mergeHooks makes root's hooks carry exactly our entries for the given specs.
// An entry of ours already there is replaced where it stands, so a second run
// changes nothing and never reorders anyone else's hooks. Each of our scripts
// (the bank hook, the memory hook) is merged on its own: installing one never
// disturbs the other, and one we no longer install is dropped.
func mergeHooks(root map[string]any, specs []hookSpec) bool {
	before, _ := json.Marshal(root)
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	for _, marker := range []string{agentMarker, contextMarker} {
		mergeMarker(hooks, specs, marker)
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}
	after, _ := json.Marshal(root)
	return !bytes.Equal(before, after)
}

func mergeMarker(hooks map[string]any, all []hookSpec, marker string) {
	wanted := map[string]bool{}
	var specs []hookSpec
	for _, s := range all {
		if s.marker == marker {
			wanted[s.event] = true
			specs = append(specs, s)
		}
	}
	for ev, groups := range hooks {
		if wanted[ev] {
			continue
		}
		// An event we no longer install (--no-tools after a full install).
		list, _ := groups.([]any)
		if kept, c := dropOwned(list, marker); c {
			if len(kept) == 0 {
				delete(hooks, ev)
			} else {
				hooks[ev] = kept
			}
		}
	}
	for _, s := range specs {
		hook := map[string]any{"type": "command", "command": s.command, "timeout": s.timeout}
		group := map[string]any{"hooks": []any{hook}}
		if s.matcher != "" {
			group = map[string]any{"matcher": s.matcher, "hooks": []any{hook}}
		}
		list, _ := hooks[s.event].([]any)
		var next []any
		placed := false
		for _, g := range list {
			if !ownsGroup(g, marker) {
				next = append(next, g)
				continue
			}
			if !placed {
				next = append(next, group)
				placed = true
			}
			// A group a person shared with our entry keeps their part.
			if kept, _ := dropOwned([]any{g}, marker); len(kept) > 0 {
				next = append(next, kept...)
			}
		}
		if !placed {
			next = append(next, group)
		}
		hooks[s.event] = next
	}
}

func removeHooks(root map[string]any) bool {
	hooks, _ := root["hooks"].(map[string]any)
	changed := false
	for ev, groups := range hooks {
		list, _ := groups.([]any)
		kept, c := dropOwned(list)
		if !c {
			continue
		}
		changed = true
		if len(kept) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = kept
		}
	}
	if changed && len(hooks) == 0 {
		delete(root, "hooks")
	}
	return changed
}

// ---- MCP entries ------------------------------------------------------------

func mcpBinary() (string, error) {
	if exe, err := os.Executable(); err == nil {
		sibling := filepath.Join(filepath.Dir(exe), "grimoire-mcp")
		if st, err := os.Stat(sibling); err == nil && !st.IsDir() {
			return sibling, nil
		}
	}
	if p, err := exec.LookPath("grimoire-mcp"); err == nil {
		abs, _ := filepath.Abs(p)
		return abs, nil
	}
	return "", errors.New("grimoire-mcp not found next to grimoire or on PATH; pass --no-mcp to silence this")
}

func ownedMCP(entry any) bool {
	m, _ := entry.(map[string]any)
	env, _ := m["env"].(map[string]any)
	v, _ := env[installedKey].(string)
	return v == installedVal
}

func installJSONMCP(path, command string, env map[string]string, dryRun bool) (string, error) {
	root, raw, err := readJSONObject(path)
	if err != nil {
		return "", err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if existing, ok := servers["grimoire"]; ok && !ownedMCP(existing) {
		return path + ` (kept your own "grimoire" server entry)`, nil
	}
	envAny := map[string]any{}
	for k, v := range env {
		envAny[k] = v
	}
	entry := map[string]any{"type": "stdio", "command": command, "args": []any{}, "env": envAny}
	before, _ := json.Marshal(servers["grimoire"])
	after, _ := json.Marshal(entry)
	servers["grimoire"] = entry
	root["mcpServers"] = servers
	next, err := encodeJSON(root)
	if err != nil {
		return "", err
	}
	return applyFile(path, raw, next, !bytes.Equal(before, after), dryRun)
}

func uninstallJSONMCP(path string, dryRun bool) (string, error) {
	root, raw, err := readJSONObject(path)
	if err != nil {
		return "", err
	}
	servers, _ := root["mcpServers"].(map[string]any)
	if !ownedMCP(servers["grimoire"]) {
		return path + " (no entry of ours)", nil
	}
	delete(servers, "grimoire")
	if len(servers) == 0 {
		delete(root, "mcpServers")
	}
	next, err := encodeJSON(root)
	if err != nil {
		return "", err
	}
	return applyFile(path, raw, next, true, dryRun)
}

func tomlString(s string) string {
	b, _ := json.Marshal(s) // a JSON string is a valid TOML basic string for these values
	return string(b)
}

func tomlBlock(command string, env map[string]string) string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var pairs []string
	for _, k := range keys {
		pairs = append(pairs, k+" = "+tomlString(env[k]))
	}
	return tomlBegin + "\n[mcp_servers.grimoire]\ncommand = " + tomlString(command) +
		"\nenv = { " + strings.Join(pairs, ", ") + " }\n" + tomlEnd + "\n"
}

func splitTOMLBlock(text string) (before, after string, found bool) {
	return splitBetween(text, tomlBegin, tomlEnd)
}

func splitBetween(text, begin, end string) (before, after string, found bool) {
	i := strings.Index(text, begin)
	j := strings.Index(text, end)
	if i < 0 || j < i {
		return text, "", false
	}
	return text[:i], strings.TrimPrefix(text[j+len(end):], "\n"), true
}

func installTOMLMCP(path, command string, env map[string]string, dryRun bool) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if errors.Is(err, os.ErrNotExist) {
		raw = nil
	}
	text := string(raw)
	before, after, found := splitTOMLBlock(text)
	if !found && strings.Contains(text, "[mcp_servers.grimoire]") {
		return path + ` (kept your own [mcp_servers.grimoire] table)`, nil
	}
	block := tomlBlock(command, env)
	var next string
	if found {
		next = before + block + after
	} else {
		sep := ""
		if text != "" && !strings.HasSuffix(text, "\n") {
			sep = "\n"
		}
		if text != "" {
			sep += "\n"
		}
		next = text + sep + block
	}
	return applyFile(path, raw, []byte(next), next != text, dryRun)
}

func uninstallTOMLMCP(path string, dryRun bool) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path + " (no entry of ours)", nil
	}
	if err != nil {
		return "", err
	}
	before, after, found := splitTOMLBlock(string(raw))
	if !found {
		return path + " (no entry of ours)", nil
	}
	next := strings.TrimRight(before, "\n")
	if next != "" && after != "" {
		next += "\n\n"
	} else if next != "" {
		next += "\n"
	}
	next += after
	return applyFile(path, raw, []byte(next), true, dryRun)
}

// codexHook is the part of a hooks/list entry the trust step needs.
type codexHook struct {
	Key         string `json:"key"`
	Command     string `json:"command"`
	SourcePath  string `json:"sourcePath"`
	CurrentHash string `json:"currentHash"`
}

// listCodexHooks asks Codex itself for its hooks and their hashes. Codex runs a
// hook only once config.toml holds a trusted_hash for it, and the hash is
// Codex's own, so the only dependable source is Codex.
func listCodexHooks(codexHome string) ([]codexHook, error) {
	bin, err := exec.LookPath("codex")
	if err != nil {
		return nil, errors.New("codex is not on PATH")
	}
	cmd := exec.Command(bin, "app-server", "--listen", "stdio://")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	type result struct {
		hooks []codexHook
		err   error
	}
	done := make(chan result, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 8<<20)
		for sc.Scan() {
			var msg struct {
				ID     int `json:"id"`
				Result struct {
					Data []struct {
						Hooks []codexHook `json:"hooks"`
					} `json:"data"`
				} `json:"result"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(sc.Bytes(), &msg) != nil || msg.ID != 2 {
				continue
			}
			if msg.Error != nil {
				done <- result{err: errors.New(msg.Error.Message)}
				return
			}
			var hooks []codexHook
			for _, d := range msg.Result.Data {
				hooks = append(hooks, d.Hooks...)
			}
			done <- result{hooks: hooks}
			return
		}
		done <- result{err: errors.New("codex app-server closed without answering")}
	}()
	for _, line := range []string{
		`{"id":1,"method":"initialize","params":{"clientInfo":{"name":"grimoire","version":"1"}}}`,
		`{"method":"initialized"}`,
		`{"id":2,"method":"hooks/list","params":{"cwds":[]}}`,
	} {
		if _, err := io.WriteString(in, line+"\n"); err != nil {
			return nil, err
		}
	}
	select {
	case r := <-done:
		return r.hooks, r.err
	case <-time.After(20 * time.Second):
		return nil, errors.New("codex app-server did not answer in 20s")
	}
}

// trustCodexHooks records Codex's own hash for each hook this command wrote, in a
// managed block of config.toml. Without it Codex 0.157+ lists the hooks as
// untrusted and silently never runs them. Only hooks carrying our marker in
// our own hooks.json are trusted; anything else Codex finds is left for you.
func trustCodexHooks(t *agentTarget, o agentOptions) (string, error) {
	if o.dryRun {
		return "would record Codex's trust for the hooks above", nil
	}
	hooks, err := listCodexHooks(t.dir)
	if err != nil {
		return "skipped (" + err.Error() + "); open /hooks in Codex and trust the grimoire hooks", nil
	}
	raw, err := os.ReadFile(t.mcpFile)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	text := string(raw)
	before, after, _ := splitBetween(text, trustBegin, trustEnd)
	outside := before + after
	var b strings.Builder
	n := 0
	for _, h := range hooks {
		if h.SourcePath != t.hooksFile || !owned(h.Command) || h.CurrentHash == "" {
			continue
		}
		if strings.Contains(outside, "[hooks.state."+tomlString(h.Key)+"]") {
			continue // the user already holds an entry for this key; leave it
		}
		fmt.Fprintf(&b, "[hooks.state.%s]\ntrusted_hash = %s\n", tomlString(h.Key), tomlString(h.CurrentHash))
		n++
	}
	next := strings.TrimRight(before, "\n")
	if next != "" {
		next += "\n\n"
	}
	if n > 0 {
		next += trustBegin + "\n" + b.String() + trustEnd + "\n"
	}
	if after = strings.TrimLeft(after, "\n"); after != "" {
		next += "\n" + after
	}
	if n == 0 && strings.TrimSpace(next) == "" {
		next = ""
	}
	msg, err := applyFile(t.mcpFile, raw, []byte(next), next != text, false)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%d hook(s) trusted in %s", n, msg), nil
}

func uninstallTrust(path string, dryRun bool) (string, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return path + " (no entry of ours)", nil
	}
	if err != nil {
		return "", err
	}
	before, after, found := splitBetween(string(raw), trustBegin, trustEnd)
	if !found {
		return path + " (no entry of ours)", nil
	}
	after = strings.TrimLeft(after, "\n")
	next := strings.TrimRight(before, "\n")
	if next != "" && after != "" {
		next += "\n\n"
	} else if next != "" {
		next += "\n"
	}
	next += after
	return applyFile(path, raw, []byte(next), true, dryRun)
}
