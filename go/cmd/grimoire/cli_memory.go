package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/convo"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// The terminal half of agent memory.
//
// Memory is written by agents over MCP and read by them over HTTP, which
// leaves the person whose memory it is with a web console and a text editor.
// These commands are the third way in: check what an agent believes, correct
// it, and retract something, from the shell — without a server running, since
// they go through the same handlers in process.

func cmdRemember(args []string) int {
	text := strings.TrimSpace(stdinOrArgs(positional(args)))
	if text == "" {
		return fail("usage: grimoire remember TEXT [--topic T] [--session S] " +
			"[--category C] [--expires-in 72h] [--immutable] [--human]")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	body := map[string]any{"text": text, "agent": flagOr(args, "--agent", "cli")}
	for flag, field := range map[string]string{
		"--topic": "topic", "--task": "task", "--session": "session",
		"--category": "category", "--expires-in": "expires_in",
	} {
		if v, ok := flagValue(args, flag); ok {
			body[field] = v
		}
	}
	if hasFlag(args, "--immutable") {
		body["immutable"] = true
	}
	if hasFlag(args, "--human") {
		body["human"] = true
	}
	if hasFlag(args, "--verbatim") {
		body["infer"] = false
	}
	status, raw := e.callBody("POST", "/api/memory", body)
	if status != http.StatusCreated {
		return fail("remember failed: %s", raw)
	}
	var out struct {
		Path    string `json:"path"`
		Results []struct {
			Op, ID, Text, Target, Why string
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	// The operation is the interesting part: a write that superseded an older
	// belief, or that changed nothing, should not read as a plain success.
	for _, r := range out.Results {
		switch r.Op {
		case "NOOP":
			fmt.Printf("unchanged  already recorded — %s\n", r.Why)
		case "DELETE":
			fmt.Printf("retracted  %s\n", r.Why)
		case "UPDATE":
			fmt.Printf("superseded %s\n           %s [%s]\n", r.Why, out.Path, r.ID)
		default:
			fmt.Printf("remembered %s [%s]\n", out.Path, r.ID)
		}
	}
	return 0
}

func cmdRecall(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	q := url.Values{}
	q.Set("limit", flagOr(args, "--limit", "20"))
	if query := strings.TrimSpace(strings.Join(positional(args), " ")); query != "" {
		q.Set("q", query)
	}
	for flag, param := range map[string]string{
		"--agent": "agent", "--session": "session", "--category": "category",
		"--as-of": "as_of",
	} {
		if v, ok := flagValue(args, flag); ok {
			q.Set(param, v)
		}
	}
	if hasFlag(args, "--all") {
		q.Set("include_superseded", "1")
		q.Set("include_expired", "1")
	}
	explain := hasFlag(args, "--why")
	if explain {
		q.Set("explain", "1")
	}
	status, raw := e.call("GET", "/api/memory?"+q.Encode())
	if status != http.StatusOK {
		return fail("recall failed: %s", raw)
	}
	var facts []struct {
		ID, Text, Path, Agent, Session, Category, Stamp string
		SupersededBy                                    string `json:"superseded_by"`
		Authority                                       string `json:"authority"`
		Score                                           float64
		Scores                                          map[string]float64
	}
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return fail("%v", err)
	}
	if len(facts) == 0 {
		fmt.Println("nothing recorded")
		return 0
	}
	for _, f := range facts {
		mark := " "
		switch {
		case f.SupersededBy != "":
			mark = "×" // replaced later; shown only with --all
		case f.Authority == "human":
			// A person asserted this. Worth a column of its own: when two
			// facts disagree, which one is yours is the first thing you want
			// to know, and it is the one an agent may not overwrite.
			mark = "✎"
		case f.Authority == "pulled":
			mark = "~" // came from text other people can write
		}
		fmt.Printf("%s %-12s %s\n", mark, f.ID, f.Text)
		meta := []string{f.Stamp}
		for _, s := range []string{f.Agent, f.Session, f.Category} {
			if s != "" {
				meta = append(meta, s)
			}
		}
		fmt.Printf("  %-12s %s · %s\n", "", strings.Join(meta, " · "), f.Path)
		if explain {
			fmt.Printf("  %-12s score %.3f (semantic %.2f · keyword %.2f · entity %.2f · recency %.2f)\n",
				"", f.Score, f.Scores["semantic"], f.Scores["keyword"],
				f.Scores["entity"], f.Scores["recency"])
		}
	}
	return 0
}

func cmdForget(args []string) int {
	pos := positional(args)
	if len(pos) < 2 {
		return fail("usage: grimoire forget PATH ID [--hard]   (ids come from `grimoire recall`)")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	q := url.Values{}
	q.Set("path", pos[0])
	q.Set("id", pos[1])
	q.Set("agent", flagOr(args, "--agent", "cli"))
	if hasFlag(args, "--hard") {
		q.Set("hard", "1")
	}
	status, raw := e.call("DELETE", "/api/memory/entry?"+q.Encode())
	if status != http.StatusOK {
		return fail("forget failed: %s", raw)
	}
	if hasFlag(args, "--hard") {
		fmt.Printf("removed %s from %s\n", pos[1], pos[0])
	} else {
		fmt.Printf("retracted %s — still in %s, struck through\n", pos[1], pos[0])
	}
	return 0
}

// flagOr reads a flag with a default.
func flagOr(args []string, name, def string) string {
	if v, ok := flagValue(args, name); ok {
		return v
	}
	return def
}

// positional drops flags and their values, leaving the words a command's text
// is built from. Without it `grimoire remember the box is fast --topic ops`
// remembers the flag as part of the fact.
func positional(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "--") {
			out = append(out, a)
			continue
		}
		if valuedFlags[a] && i+1 < len(args) {
			i++ // skip the value too
		}
	}
	return out
}

// valuedFlags are the flags that take a value, so positional() knows which
// following word to skip. A boolean flag's neighbour is a word, not a value.
var valuedFlags = map[string]bool{
	"--topic": true, "--task": true, "--session": true, "--category": true,
	"--expires-in": true, "--agent": true, "--limit": true, "--as-of": true,
}

// cmdChallenges lists the disagreements an agent has raised against facts a
// person recorded, and settles them.
//
// The list is the point: a refusal nobody sees is a refusal nobody can act on,
// and the agent may well be right — the operator's fact can be the stale one.
func cmdChallenges(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	for flag, res := range map[string]string{"--uphold": "uphold", "--concede": "concede"} {
		id, ok := flagValue(args, flag)
		if !ok {
			continue
		}
		note, ok := flagValue(args, "--note")
		if !ok {
			return fail("%s needs --note PATH (the note the fact lives in; " +
				"`grimoire challenges` prints it)")
		}
		status, raw := e.callBody("POST", "/api/memory/challenge",
			map[string]any{"note": note, "id": id, "resolution": res})
		if status != http.StatusOK {
			return fail("resolve failed: %s", raw)
		}
		fmt.Printf("%s: %s\n", res, raw)
		return 0
	}

	status, raw := e.call("GET", "/api/memory/challenges")
	if status != http.StatusOK {
		return fail("challenges failed: %s", raw)
	}
	var open []struct {
		ID, Text, Agent, Stamp, Note string
		ContestedID                  string `json:"contested_id"`
		ContestedText                string `json:"contested_text"`
		ContestedAuthority           string `json:"contested_authority"`
	}
	if err := json.Unmarshal([]byte(raw), &open); err != nil {
		return fail("%v", err)
	}
	if len(open) == 0 {
		fmt.Println("no open challenges")
		return 0
	}
	for _, c := range open {
		fmt.Printf("%-12s %s\n", c.ID, c.Text)
		fmt.Printf("  %-10s contests %s (%s): %s\n", "", c.ContestedID,
			c.ContestedAuthority, c.ContestedText)
		fmt.Printf("  %-10s %s · %s · %s\n", "", c.Stamp, c.Agent, c.Note)
	}
	fmt.Printf("\n%d open. Settle with:\n"+
		"  grimoire challenges --note PATH --uphold ID    (your fact stands)\n"+
		"  grimoire challenges --note PATH --concede ID   (the agent was right)\n",
		len(open))
	return 0
}

// cmdDoctor diagnoses the disagreements that make an agent look ignorant.
//
// Every failure worth a command like this is silent: the vault, the index and
// what an agent can reach stop agreeing, nothing errors, and the symptom is
// "the agent doesn't know that" rather than a stack trace.
func cmdDoctor(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	status, raw := e.call("GET", "/api/doctor")
	if status != http.StatusOK {
		return fail("doctor failed: %s", raw)
	}
	var out struct {
		Status string `json:"status"`
		Checks []struct {
			Name, Status, Detail, Fix string
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return fail("%v", err)
	}
	mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}
	worst := 0
	for _, c := range out.Checks {
		fmt.Printf("%s %-22s %s\n", mark[c.Status], c.Name, c.Detail)
		if c.Fix != "" {
			fmt.Printf("  %-22s → %s\n", "", c.Fix)
		}
		if c.Status == "fail" {
			worst = 1
		}
	}
	fmt.Printf("\n%s\n", out.Status)
	// A non-zero exit on failure, so this is usable from a healthcheck or a
	// unit file rather than only by a person reading it.
	return worst
}

// cmdImport turns a ChatGPT or Claude export into notes.
//
// The cold start is the adoption problem: an empty vault answers nothing, and
// "point it at the markdown you already have" does not help someone whose
// knowledge is two years of chat history in a zip. This is the other thing they
// already have.
func cmdImport(args []string) int {
	pos := positional(args)
	if len(pos) == 0 {
		return fail("usage: grimoire import PATH [--into SUBDIR] [--dry-run]\n" +
			"  PATH is conversations.json from a ChatGPT or Claude data export.")
	}
	src := pos[0]
	if strings.HasPrefix(src, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			src = filepath.Join(home, src[2:])
		}
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return fail("cannot read %s: %v", src, err)
	}

	// Detected from the shape, not the filename: both products call the file
	// conversations.json, and somebody who renamed it should not have to
	// remember which flag to pass.
	kind := convo.Detect(raw)
	var convos []convo.Conversation
	switch kind {
	case "chatgpt":
		convos, err = convo.ParseChatGPT(raw)
	case "claude":
		convos, err = convo.ParseClaude(raw)
	default:
		return fail("%s does not look like a ChatGPT or Claude conversations.json", src)
	}
	if err != nil {
		return fail("%v", err)
	}
	if len(convos) == 0 {
		return fail("no conversations with any content in %s", src)
	}

	into := "conversations/" + kind
	if v, ok := flagValue(args, "--into"); ok {
		into = strings.Trim(v, "/")
	}
	dry := hasFlag(args, "--dry-run")
	fmt.Printf("%s export: %d conversations → %s/\n", kind, len(convos), into)
	if dry {
		for i, c := range convos {
			if i >= 10 {
				fmt.Printf("  … and %d more\n", len(convos)-10)
				break
			}
			fmt.Printf("  %-52s %d messages\n", truncTitle(c.Title), len(c.Messages))
		}
		return 0
	}

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	written := 0
	for _, c := range convos {
		title := c.Title
		if title == "" {
			title = "Untitled conversation"
		}
		fm := map[string]any{
			"title":  title,
			"tags":   []string{"conversation", kind},
			"origin": "import:" + kind,
		}
		if !c.Created.IsZero() {
			fm["created"] = c.Created.Format("2006-01-02T15:04:05")
		}
		body := c.Markdown()
		status, raw := e.callBody("POST", "/api/notes", map[string]any{
			"path": filepath.Join(into, vault.Slugify(title)+".md"),
			"body": body, "frontmatter": fm,
		})
		if status >= 400 {
			fmt.Fprintf(os.Stderr, "  skipped %q: %s\n", truncTitle(title), raw)
			continue
		}
		written++
	}
	fmt.Printf("imported %d conversation(s)\n", written)
	return 0
}

func truncTitle(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) > 50 {
		return s[:49] + "…"
	}
	return s
}
