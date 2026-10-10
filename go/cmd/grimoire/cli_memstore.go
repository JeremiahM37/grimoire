package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream/filemem"
	"github.com/JeremiahM37/grimoire/go/internal/impact"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
)

// The shared memory store from the terminal: one canonical directory every
// agent's own memory location points at, a generated MEMORY.md of rules plus
// topic pointers, and a status view of who is linked. See docs/MEMORY_STORE.md.

const memoryUsage = `usage:
` + memoryPortabilityUsage + `
  grimoire memory receipts                 forget receipts (from forget --cascade)
  grimoire memory prefix [--since TOKEN] [--max-tokens N]   durable-memory block for a cached prompt prefix
  grimoire memory disputes                 facts a person recorded that an agent contests
  grimoire memory resolve ID keep|accept|merge [text] [--challenger ID] [--path P]
  grimoire memory image FILE [--caption TEXT] [--topic T]  store a picture (caption-only retrieval)
  grimoire memory link --agent NAME --path P [--kind dir|file] [--dir STORE] [--merge] [--dry-run]
  grimoire memory unlink --agent NAME [--path P] [--dry-run]
  grimoire memory status [--dir STORE]
  grimoire memory index [--dir STORE] [--write] [--budget BYTES]
  grimoire memory core [--budget BYTES] [--format md|text]
  grimoire memory replay [--diff --note P --with FILE | --change FILE | --index | seed FILE] [--json]
  grimoire memory index --write [--force]   (replays the new index first; held if it loses useful recalls)
  grimoire memory impact [--since 90d] [--agent NAME] [--min N] [--json]
  grimoire memory trace [--target fact:ID|note:PATH] [--days 30] [--json]
  grimoire memory impact --retells [--cutoff 2026-10-09] [--budget 2000] [--since D] [--all-sessions] [--json]
  grimoire memory prune [--dry-run | --apply] [--max N] [--below 1-3] [--json]
  grimoire memory watch [--agent NAME] [--session ID]   (live: learned, superseded, forgotten, challenged, disputed)
  grimoire memory profile [--subject user|agent] [--agent NAME] [--budget TOKENS] [--synthesize] [--json]

A directory memory (Claude Code's memory/) becomes a symlink to the store; a
single file (AGENTS.md, GEMINI.md) gets a managed block holding the core.
Nothing is deleted: the old directory or file is kept as a .grimoire-bak copy.`

// memoryLink is one registered agent location.
type memoryLink struct {
	Agent string `json:"agent"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
}

func cmdMemory(args []string) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println(memoryUsage)
		return 0
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	rest := args[1:]
	switch args[0] {
	case "link":
		return memoryLinkCmd(e, rest)
	case "unlink":
		return memoryUnlinkCmd(e, rest)
	case "status":
		return memoryStatusCmd(e, rest)
	case "index":
		return memoryIndexCmd(e, rest)
	case "core":
		return memoryCoreCmd(e, rest)
	case "impact":
		return memoryImpactCmd(e, rest)
	case "trace":
		return memoryTraceCmd(e, rest)
	case "replay":
		return memoryReplayCmd(e, rest)
	case "export":
		return memoryExportCmd(e, rest)
	case "import":
		return memoryImportCmd(e, rest)
	case "prune":
		return memoryPruneCmd(e, rest)
	case "watch":
		return memoryWatchCmd(e, rest)
	case "profile":
		return memoryProfileCmd(e, rest)
	case "prefix":
		return memoryPrefixCmd(e, rest)
	case "receipts":
		return memoryReceiptsCmd(e, rest)
	case "disputes":
		return memoryDisputesCmd(e, rest)
	case "resolve":
		return memoryResolveCmd(e, rest)
	case "image":
		return memoryImageCmd(e, rest)
	}
	return fail("unknown memory command %q\n\n%s", args[0], memoryUsage)
}

// storeDir resolves the canonical store: --dir, the setting, else
// ~/.grimoire/memory.
func (e *env) storeDir(args []string) string {
	dir := flagOr(args, "--dir", "")
	if dir == "" {
		dir = e.settings.Get("memory_canonical_dir")
	}
	if dir == "" {
		dir = e.settings.Get("dream_canonical_memory")
	}
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".grimoire", "memory")
	}
	return expandPath(dir)
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		p = filepath.Join(os.Getenv("HOME"), p[2:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func (e *env) linksPath() string {
	return filepath.Join(e.vault.Root, ".grimoire", "memory-links.json")
}

func (e *env) loadLinks() []memoryLink {
	var out []memoryLink
	if b, err := os.ReadFile(e.linksPath()); err == nil {
		_ = json.Unmarshal(b, &out)
	}
	return out
}

func (e *env) saveLinks(links []memoryLink) error {
	sort.Slice(links, func(i, j int) bool { return links[i].Agent+links[i].Path < links[j].Agent+links[j].Path })
	b, _ := json.MarshalIndent(links, "", "  ")
	if err := os.MkdirAll(filepath.Dir(e.linksPath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(e.linksPath(), b, 0o600)
}

// blockText is the core a file-shaped target holds: plain lines, small.
func (e *env) blockText(store string, budget int) string {
	notes, err := memstore.LoadDir(store)
	if err != nil {
		return ""
	}
	opt := memstore.CoreOptions{Budget: budget}
	if e.embedder != nil {
		opt.Embed = e.embedder.Embed
	}
	return memstore.BuildCore(notes, opt).Text
}

func memoryLinkCmd(e *env, args []string) int {
	agent, p := flagOr(args, "--agent", ""), flagOr(args, "--path", "")
	if agent == "" || p == "" {
		return fail("usage: grimoire memory link --agent NAME --path P [--kind dir|file] [--merge]")
	}
	kind := flagOr(args, "--kind", "")
	p = expandPath(p)
	if kind == "" {
		kind = memstore.ShapeDir
		if fi, err := os.Stat(p); (err == nil && !fi.IsDir()) || strings.HasSuffix(p, ".md") {
			kind = memstore.ShapeFile
		}
	}
	store := e.storeDir(args)
	opt := memstore.LinkOptions{Agent: agent, Merge: hasFlag(args, "--merge"), DryRun: hasFlag(args, "--dry-run")}
	if kind == memstore.ShapeFile {
		if _, err := os.Stat(store); err != nil {
			return fail("the store %s does not exist yet; link a directory agent first or create it", store)
		}
		opt.Block = e.blockText(store, 6000)
	}
	res, err := memstore.Link(kind, p, store, opt)
	if err != nil {
		return fail("%v", err)
	}
	prefix := ""
	if opt.DryRun {
		prefix = "(dry run) "
	}
	fmt.Printf("%s%s: %s %s\n", prefix, agent, res.Action, p)
	for _, m := range res.Moved {
		fmt.Printf("  moved into the store: %s\n", m)
	}
	for _, m := range res.Deduped {
		fmt.Printf("  already in the store, identical: %s\n", m)
	}
	for _, m := range res.Renamed {
		fmt.Printf("  differed, kept both: %s\n", m)
	}
	if res.Backup != "" {
		fmt.Printf("  backup: %s\n", res.Backup)
	}
	if !opt.DryRun {
		links := e.loadLinks()
		kept := links[:0]
		for _, l := range links {
			if !(l.Agent == agent && l.Path == p) {
				kept = append(kept, l)
			}
		}
		if err := e.saveLinks(append(kept, memoryLink{Agent: agent, Kind: kind, Path: p})); err != nil {
			return fail("linked, but could not record it: %v", err)
		}
	}
	return 0
}

func memoryUnlinkCmd(e *env, args []string) int {
	agent, p := flagOr(args, "--agent", ""), flagOr(args, "--path", "")
	if agent == "" {
		return fail("usage: grimoire memory unlink --agent NAME [--path P]")
	}
	store := e.storeDir(args)
	links := e.loadLinks()
	var kept []memoryLink
	n := 0
	for _, l := range links {
		if l.Agent != agent || (p != "" && l.Path != expandPath(p)) {
			kept = append(kept, l)
			continue
		}
		n++
		what, err := memstore.Unlink(l.Kind, l.Path, store, memstore.LinkOptions{DryRun: hasFlag(args, "--dry-run")})
		if err != nil {
			return fail("%s: %v", l.Path, err)
		}
		fmt.Printf("%s: %s %s\n", agent, what, l.Path)
	}
	if n == 0 {
		return fail("no link recorded for agent %q", agent)
	}
	if !hasFlag(args, "--dry-run") {
		if err := e.saveLinks(kept); err != nil {
			return fail("%v", err)
		}
	}
	return 0
}

func memoryStatusCmd(e *env, args []string) int {
	store := e.storeDir(args)
	notes, err := memstore.LoadDir(store)
	counts := map[string]int{}
	for _, n := range notes {
		counts[n.Kind]++
	}
	if err != nil {
		fmt.Printf("store   %s (not created yet)\n", store)
	} else {
		var parts []string
		for _, k := range memstore.Kinds {
			if counts[k] > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
			}
		}
		fmt.Printf("store   %s: %d notes (%s)\n", store, len(notes), strings.Join(parts, ", "))
	}
	links := e.loadLinks()
	if len(links) == 0 {
		fmt.Println("no agents linked; link one with: grimoire memory link --agent NAME --path P")
		return 0
	}
	want := e.blockText(store, 6000)
	for _, l := range links {
		st := memstore.Status(l.Kind, l.Path, store, want)
		line := fmt.Sprintf("%-12s %-5s %-16s %s", l.Agent, l.Kind, st.State, l.Path)
		if st.Notes > 0 && st.State != "linked" {
			line += fmt.Sprintf("  (%d notes)", st.Notes)
		}
		fmt.Println(line)
	}
	return 0
}

// buildIndexText is the MEMORY.md `memory index` would write.
func buildIndexText(e *env, store string, args []string) (string, error) {
	notes, err := memstore.LoadDir(store)
	if err != nil {
		return "", err
	}
	opt := memstore.CoreOptions{Links: true}
	if v := flagOr(args, "--budget", ""); v != "" {
		fmt.Sscanf(v, "%d", &opt.Budget)
	}
	if e.embedder != nil {
		opt.Embed = e.embedder.Embed
	}
	return memstore.BuildCore(notes, opt).Text, nil
}

func memoryIndexCmd(e *env, args []string) int {
	store := e.storeDir(args)
	notes, err := memstore.LoadDir(store)
	if err != nil {
		return fail("%v", err)
	}
	opt := memstore.CoreOptions{Links: true}
	if v := flagOr(args, "--budget", ""); v != "" {
		fmt.Sscanf(v, "%d", &opt.Budget)
	}
	if e.embedder != nil {
		opt.Embed = e.embedder.Embed
	}
	core := memstore.BuildCore(notes, opt)
	if !hasFlag(args, "--write") {
		fmt.Print(core.Text)
		fmt.Fprintf(os.Stderr, "\n%d lines, %d bytes: %d of %d rules, %d pointers covering %d notes (dry run; --write to save)\n",
			core.Lines, core.Bytes, core.RulesShown, core.Rules, core.Pointers, core.Notes)
		return 0
	}
	target := filepath.Join(store, memstore.IndexName)
	if !hasFlag(args, "--force") {
		if reason := indexHeld(e, target, core.Text); reason != "" {
			fmt.Fprintf(os.Stderr, "not written: %s\nreview with: grimoire memory replay --diff --index   (or rerun with --force)\n", reason)
			return 2
		}
	}
	if old, err := os.ReadFile(target); err == nil {
		if strings.Contains(string(old), filemem.GeneratedMarker) && string(old) == core.Text {
			fmt.Println("MEMORY.md already up to date")
		} else {
			backup := target + ".grimoire-bak-" + time.Now().Format("20060102-150405")
			if err := os.WriteFile(backup, old, 0o600); err != nil {
				return fail("%v", err)
			}
			fmt.Printf("backup: %s\n", backup)
		}
	}
	if err := os.WriteFile(target, []byte(core.Text), 0o644); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("wrote %s: %d lines, %d bytes; %d of %d rules, %d pointers covering %d notes\n",
		target, core.Lines, core.Bytes, core.RulesShown, core.Rules, core.Pointers, core.Notes)
	// Keep every linked single-file target current.
	block := e.blockText(store, 6000)
	for _, l := range e.loadLinks() {
		if l.Kind != memstore.ShapeFile {
			continue
		}
		res, err := memstore.Link(memstore.ShapeFile, l.Path, store, memstore.LinkOptions{Agent: l.Agent, Block: block})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", l.Path, err)
			continue
		}
		fmt.Printf("%s: %s %s\n", l.Agent, res.Action, l.Path)
	}
	return 0
}

func memoryCoreCmd(e *env, args []string) int {
	q := url.Values{}
	if v := flagOr(args, "--budget", ""); v != "" {
		q.Set("budget", v)
	}
	if v := flagOr(args, "--format", ""); v != "" {
		q.Set("format", v)
	}
	q.Set("raw", "1")
	status, raw := e.call("GET", "/api/memory/core?"+q.Encode())
	if status != http.StatusOK {
		return fail("core failed: %s", raw)
	}
	fmt.Print(raw)
	return 0
}

// memoryImpactCmd prints session friction before and after each memory and
// exported skill landed: medians, with session counts, labelled correlation.
func memoryImpactCmd(e *env, args []string) int {
	q := url.Values{}
	for flag, param := range map[string]string{"--since": "since", "--agent": "agent", "--min": "min", "--limit": "limit"} {
		if v := flagOr(args, flag, ""); v != "" {
			q.Set(param, v)
		}
	}
	retells := hasFlag(args, "--retells")
	if retells {
		q.Set("retells", "1")
		if hasFlag(args, "--all-sessions") {
			q.Set("all", "1")
		}
		for flag, param := range map[string]string{"--cutoff": "cutoff", "--budget": "budget"} {
			if v := flagOr(args, flag, ""); v != "" {
				q.Set(param, v)
			}
		}
	}
	status, raw := e.call("GET", "/api/memory/impact?"+q.Encode())
	if status != http.StatusOK {
		return fail("impact failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	var out struct {
		Report  impact.Report       `json:"report"`
		Retells impact.RetellReport `json:"retells"`
		Errors  []string            `json:"errors"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	if retells {
		fmt.Print(impact.RetellText(out.Retells))
		for _, msg := range out.Errors {
			fmt.Fprintln(os.Stderr, "note:", msg)
		}
		return 0
	}
	fmt.Print(impact.Text(out.Report))
	for _, msg := range out.Errors {
		fmt.Fprintln(os.Stderr, "note:", msg)
	}
	return 0
}

// indexHeld replays the regenerated index against past situations and returns
// why it must not be written automatically, or "". A server that cannot
// replay (no corpus, nothing indexed) never holds the write.
func indexHeld(e *env, target, text string) string {
	old, err := os.ReadFile(target)
	if err != nil || string(old) == text {
		return ""
	}
	status, out := e.callBody("POST", "/api/memory/replay",
		map[string]any{"notes": []map[string]string{{"path": target, "text": text}}})
	if status != http.StatusOK {
		return "" // the index is not an indexed note here; nothing to replay
	}
	var res struct {
		Verdict struct {
			Hold   bool   `json:"hold"`
			Reason string `json:"reason"`
		} `json:"verdict"`
	}
	if json.Unmarshal([]byte(out), &res) != nil || !res.Verdict.Hold {
		return ""
	}
	return "memory replay: " + res.Verdict.Reason
}
