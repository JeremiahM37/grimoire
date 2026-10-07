package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/agenthook"
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

  grimoire agent install [--claude-code] [--codex] [--bank NAME] [--url URL]
                         [--recall] [--no-mcp] [--no-tools] [--dry-run]
  grimoire agent uninstall [--claude-code] [--codex] [--dry-run]
  grimoire agent status [--claude-code] [--codex]

With neither --claude-code nor --codex, the agents whose config folder exists
(~/.claude, ~/.codex) are used. install merges, never replaces: your other hooks
and servers are untouched, a changed file is first copied to FILE.grimoire-backup-TIME,
and running it again changes nothing. --dry-run prints what would change and
writes nothing. No token is written anywhere; the agent's environment supplies it.

Hooks: SessionStart (inject the bank's rules, knowledge and the last session's
digest), PostToolUse (local activity buffer), Stop and SessionEnd (retain the
session and write its digest). --recall adds UserPromptSubmit recall.
Files: Claude Code ~/.claude/settings.json and ~/.claude.json; Codex ~/.codex/hooks.json
and ~/.codex/config.toml.`

// agentMarker identifies entries this command wrote.
const (
	agentMarker  = agenthook.FileName
	tomlBegin    = "# >>> grimoire agent install (managed block; `grimoire agent uninstall` removes it) >>>"
	tomlEnd      = "# <<< grimoire agent install <<<"
	installedKey = "GRIMOIRE_INSTALLED_BY"
	installedVal = "grimoire agent install"
)

type agentTarget struct {
	name        string // claude-code | codex
	dir         string // config folder
	hooksFile   string
	mcpFile     string
	events      func(command string, recall bool, tools bool) []hookSpec
	mcpIsTOML   bool
	hookHarness string
}

type hookSpec struct {
	event, matcher string
	timeout        int
}

func agentTargets(home string) map[string]*agentTarget {
	return map[string]*agentTarget{
		"claude-code": {name: "claude-code", dir: filepath.Join(home, ".claude"),
			hooksFile: filepath.Join(home, ".claude", "settings.json"),
			mcpFile:   filepath.Join(home, ".claude.json"), hookHarness: "claude-code",
			events: func(_ string, recall, tools bool) []hookSpec {
				specs := []hookSpec{{"SessionStart", "startup|resume|clear|compact", 8}}
				if tools {
					specs = append(specs, hookSpec{"PostToolUse", "Edit|MultiEdit|Write|NotebookEdit|Bash", 5})
				}
				specs = append(specs, hookSpec{"Stop", "", 20}, hookSpec{"SessionEnd", "", 60})
				if recall {
					specs = append(specs, hookSpec{"UserPromptSubmit", "", 4})
				}
				return specs
			}},
		"codex": {name: "codex", dir: filepath.Join(home, ".codex"),
			hooksFile: filepath.Join(home, ".codex", "hooks.json"),
			mcpFile:   filepath.Join(home, ".codex", "config.toml"), mcpIsTOML: true, hookHarness: "codex",
			events: func(_ string, recall, tools bool) []hookSpec {
				specs := []hookSpec{{"SessionStart", "startup|resume|clear", 8}}
				if tools {
					specs = append(specs, hookSpec{"PostToolUse", "Bash|apply_patch", 5})
				}
				// Codex has no SessionEnd: Stop does both jobs.
				specs = append(specs, hookSpec{"Stop", "", 60})
				if recall {
					specs = append(specs, hookSpec{"UserPromptSubmit", "", 4})
				}
				return specs
			}},
	}
}

// agentPlan is what one run intends for one file.
type agentPlan struct {
	path    string
	content []byte // the new content; nil means remove nothing / no write
	changed bool
	notes   []string
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
	if action != "install" && action != "uninstall" && action != "status" {
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
	targets := agentTargets(home)
	var chosen []*agentTarget
	for _, name := range []string{"claude-code", "codex"} {
		if f.on["--"+name] {
			chosen = append(chosen, targets[name])
		}
	}
	if len(chosen) == 0 {
		for _, name := range []string{"claude-code", "codex"} {
			if st, err := os.Stat(targets[name].dir); err == nil && st.IsDir() {
				chosen = append(chosen, targets[name])
			}
		}
	}
	if len(chosen) == 0 {
		return fail("no agent found: neither ~/.claude nor ~/.codex exists. Pass --claude-code or --codex.")
	}
	opts := agentOptions{
		bank: f.str("--bank", ""), url: f.str("--url", os.Getenv("GRIMOIRE_URL")),
		recall: f.on["--recall"], noMCP: f.on["--no-mcp"], noTools: f.on["--no-tools"],
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
	bank, url, home                string
	recall, noMCP, noTools, dryRun bool
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
	env = append(env, "GRIMOIRE_BANK_HARNESS="+t.hookHarness)
	if o.bank != "" {
		env = append(env, "GRIMOIRE_BANK="+agentQuote(o.bank))
	}
	if o.url != "" {
		env = append(env, "GRIMOIRE_URL="+agentQuote(o.url))
	}
	return strings.Join(env, " ") + " python3 " + agentQuote(script)
}

func installAgent(t *agentTarget, o agentOptions) ([]string, error) {
	var out []string
	script := filepath.Join(o.home, ".grimoire", "hooks", agenthook.FileName)
	if cur, err := os.ReadFile(script); err != nil || !bytes.Equal(cur, agenthook.Script) {
		out = append(out, "hook script: "+script+" (written)")
		if !o.dryRun {
			if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
				return out, err
			}
			if err := writeAtomic(script, agenthook.Script, 0o755); err != nil {
				return out, err
			}
		}
	} else {
		out = append(out, "hook script: "+script+" (up to date)")
	}

	command := hookCommand(t, o, script)
	root, raw, err := readJSONObject(t.hooksFile)
	if err != nil {
		return out, err
	}
	changed := mergeHooks(root, t.events(command, o.recall, !o.noTools), command)
	next, err := encodeJSON(root)
	if err != nil {
		return out, err
	}
	msg, err := applyFile(t.hooksFile, raw, next, changed, o.dryRun)
	out = append(out, "hooks: "+msg)
	if err != nil {
		return out, err
	}

	if o.noMCP {
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
	var events []string
	if hooks, ok := root["hooks"].(map[string]any); ok {
		for ev, groups := range hooks {
			if groupsHaveMarker(groups) {
				events = append(events, ev)
			}
		}
	}
	sort.Strings(events)
	out := []string{"hooks: " + orNone(strings.Join(events, ", "))}
	mcp := "not installed"
	if t.mcpIsTOML {
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
		if err := writeAtomic(backup, old, 0o600); err != nil {
			return msg + ")", fmt.Errorf("backing up %s: %w", path, err)
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

func groupsHaveMarker(groups any) bool {
	list, _ := groups.([]any)
	for _, g := range list {
		gm, _ := g.(map[string]any)
		hs, _ := gm["hooks"].([]any)
		for _, h := range hs {
			hm, _ := h.(map[string]any)
			if c, _ := hm["command"].(string); strings.Contains(c, agentMarker) {
				return true
			}
		}
	}
	return false
}

// dropOwned removes our hook entries from one event's groups, and reports
// whether anything went.
func dropOwned(groups []any) ([]any, bool) {
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
			if c, _ := hm["command"].(string); strings.Contains(c, agentMarker) {
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
func ownsGroup(g any) bool { return groupsHaveMarker([]any{g}) }

// mergeHooks makes root's hooks carry exactly our entries for the given
// events. An entry of ours already there is replaced where it stands, so a
// second run changes nothing and never reorders anyone else's hooks.
func mergeHooks(root map[string]any, specs []hookSpec, command string) bool {
	before, _ := json.Marshal(root)
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	wanted := map[string]hookSpec{}
	for _, s := range specs {
		wanted[s.event] = s
	}
	for ev, groups := range hooks {
		if _, ok := wanted[ev]; ok {
			continue
		}
		// An event we no longer install (--no-tools after a full install).
		list, _ := groups.([]any)
		if kept, c := dropOwned(list); c {
			if len(kept) == 0 {
				delete(hooks, ev)
			} else {
				hooks[ev] = kept
			}
		}
	}
	for _, s := range specs {
		hook := map[string]any{"type": "command", "command": command, "timeout": s.timeout}
		group := map[string]any{"hooks": []any{hook}}
		if s.matcher != "" {
			group = map[string]any{"matcher": s.matcher, "hooks": []any{hook}}
		}
		list, _ := hooks[s.event].([]any)
		var next []any
		placed := false
		for _, g := range list {
			if !ownsGroup(g) {
				next = append(next, g)
				continue
			}
			if !placed {
				next = append(next, group)
				placed = true
			}
			// A group a person shared with our entry keeps their part.
			if kept, _ := dropOwned([]any{g}); len(kept) > 0 {
				next = append(next, kept...)
			}
		}
		if !placed {
			next = append(next, group)
		}
		hooks[s.event] = next
	}
	root["hooks"] = hooks
	after, _ := json.Marshal(root)
	return !bytes.Equal(before, after)
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
	i := strings.Index(text, tomlBegin)
	j := strings.Index(text, tomlEnd)
	if i < 0 || j < i {
		return text, "", false
	}
	return text[:i], strings.TrimPrefix(text[j+len(tomlEnd):], "\n"), true
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
