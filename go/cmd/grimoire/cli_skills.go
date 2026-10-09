package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
	"github.com/JeremiahM37/grimoire/go/internal/api"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillexport"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

const skillsUsage = `usage:
  grimoire skills export --agent NAME [--dry-run] [--remove] [--force] [--dest DIR] [--dir STORE]
  grimoire skills mine [--agent NAME]... [--since 30d] [--min-sessions 3] [--limit 20] [--max-files 2000] [--title] [--dry-run]
  grimoire skills accept ID [--dir STORE]

export writes each procedure memory in the store as a skill folder (SKILL.md with a
name and a description built from the memory's cues, plus scripts/ when it holds shell
blocks) into the agent's skills dir (the profile's skills_dir, or --dest). A skill it
did not write is never touched; one it wrote and you edited is skipped unless --force;
--remove deletes only what it wrote.

mine reads the agents' session transcripts (profiles with a transcripts section), finds
command sequences that recur across at least --min-sessions sessions, and writes drafts
to a review queue in the vault's .grimoire/ folder. Nothing becomes a memory until you
run accept. --title lets the configured model title a candidate; it decides nothing else.`

func cmdSkills(args []string) int {
	if len(args) == 0 || args[0] == "help" {
		fmt.Println(skillsUsage)
		return 0
	}
	f, err := parseBankFlags(args[1:])
	if err != nil {
		return fail("%v", err)
	}
	switch args[0] {
	case "export":
		return skillsExport(f)
	case "mine":
		return skillsMine(f)
	case "accept":
		return skillsAccept(f)
	}
	return fail("unknown skills command %q\n\n%s", args[0], skillsUsage)
}

func skillsExport(f *bankFlags) int {
	name := f.str("--agent", "")
	if name == "" {
		return fail("usage: grimoire skills export --agent NAME [--dry-run] [--remove]")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("%v", err)
	}
	p, err := agentprofile.Load(name, home)
	if err != nil {
		return fail("%v", err)
	}
	dest := p.SkillsDir
	if v := f.str("--dest", ""); v != "" {
		dest = expandPath(v)
	}
	opt := skillexport.Options{Agent: name, Dir: dest, DryRun: f.on["--dry-run"], Force: f.on["--force"]}
	prefix := ""
	if opt.DryRun {
		prefix = "(dry run) "
	}
	if f.on["--remove"] {
		res, err := skillexport.Remove(opt)
		if err != nil {
			return fail("%v", err)
		}
		printSkillResults(prefix, res)
		if len(res) == 0 {
			fmt.Println("no skills written by Grimoire for " + name + " in " + dest)
		}
		return 0
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	store := e.storeDir(storeArgs(f))
	notes, err := memstore.LoadDir(store)
	if err != nil {
		return fail("cannot read the memory store %s: %v", store, err)
	}
	res, err := skillexport.Export(notes, opt)
	if err != nil {
		return fail("%v", err)
	}
	fmt.Printf("%s%s -> %s\n", prefix, store, dest)
	printSkillResults(prefix, res)
	return 0
}

func printSkillResults(prefix string, res []skillexport.Result) {
	counts := map[string]int{}
	for _, r := range res {
		counts[r.Action]++
		line := fmt.Sprintf("%s%-9s %s", prefix, r.Action, r.Name)
		if r.Source != "" {
			line += "  <- " + r.Source
		}
		if r.Detail != "" {
			line += "  (" + r.Detail + ")"
		}
		if r.Action != "unchanged" {
			fmt.Println(line)
		}
	}
	var parts []string
	for _, k := range []string{"created", "updated", "unchanged", "skipped", "orphan", "removed"} {
		if counts[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[k], k))
		}
	}
	if len(parts) > 0 {
		fmt.Println(strings.Join(parts, ", "))
	}
}

// ---- mine ------------------------------------------------------------------

func queuePaths(e *env) (md, js string) {
	d := filepath.Join(e.vault.Root, ".grimoire")
	return filepath.Join(d, "skill-candidates.md"), filepath.Join(d, "skill-candidates.json")
}

func skillsMine(f *bankFlags) int {
	home, err := os.UserHomeDir()
	if err != nil {
		return fail("%v", err)
	}
	since, err := api.ParseSince(f.str("--since", "30d"))
	if err != nil {
		return fail("%v", err)
	}
	var profiles []*agentprofile.Profile
	if names := f.values["--agent"]; len(names) > 0 {
		for _, n := range names {
			p, err := agentprofile.Load(n, home)
			if err != nil {
				return fail("%v", err)
			}
			if p.Transcripts.Glob == "" {
				return fail("profile %q has no transcripts section (glob + format)", n)
			}
			profiles = append(profiles, p)
		}
	} else {
		all, _ := agentprofile.All(home)
		for _, p := range all {
			if p.Transcripts.Glob != "" {
				profiles = append(profiles, p)
			}
		}
	}
	if len(profiles) == 0 {
		return fail("no agent profile has a transcripts section; add one (docs/AGENTS_ANY.md)")
	}
	maxFiles := 2000
	if v := f.str("--max-files", ""); v != "" {
		fmt.Sscanf(v, "%d", &maxFiles)
	}
	var sessions []transcript.Session
	for _, p := range profiles {
		ss, errs := transcript.ReadAll(transcript.Spec{Agent: p.Name, Format: p.Transcripts.Format, Glob: p.Transcripts.Glob, Map: p.Transcripts.Map},
			transcript.Options{Since: time.Now().Add(-since), MaxFiles: maxFiles, NoAssistantText: true})
		fmt.Printf("%-12s %d sessions read\n", p.Name, len(ss))
		for i, err := range errs {
			if i == 3 {
				fmt.Fprintf(os.Stderr, "  ... and %d more unreadable\n", len(errs)-3)
				break
			}
			fmt.Fprintln(os.Stderr, "  skipped:", err)
		}
		sessions = append(sessions, ss...)
	}
	min := 3
	if v := f.str("--min-sessions", ""); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &min); err != nil || min < 2 {
			return fail("--min-sessions needs a number, 2 or more")
		}
	}
	limit := 20
	if v := f.str("--limit", ""); v != "" {
		fmt.Sscanf(v, "%d", &limit)
	}
	cands := skillmine.Mine(sessions, skillmine.Options{MinSessions: min, Limit: limit})

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if f.on["--title"] {
		if e.server == nil || e.server.AI == nil || !e.server.AI.Available() {
			fmt.Fprintln(os.Stderr, "--title: no model configured; keeping the deterministic titles")
		} else {
			skillmine.Retitle(cands, modelTitler(e))
		}
	}
	fmt.Printf("%d candidate(s) from %d sessions\n", len(cands), len(sessions))
	for _, c := range cands {
		fmt.Printf("  %s  %2d sessions  %s\n", c.ID, c.Sessions, c.Title)
	}
	if f.on["--dry-run"] || len(cands) == 0 {
		return 0
	}
	md, js := queuePaths(e)
	if err := os.MkdirAll(filepath.Dir(md), 0o700); err != nil {
		return fail("%v", err)
	}
	raw, _ := json.MarshalIndent(cands, "", "  ")
	if err := os.WriteFile(js, raw, 0o600); err != nil {
		return fail("%v", err)
	}
	if err := os.WriteFile(md, []byte(skillmine.Queue(cands, since, time.Now())), 0o600); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("review queue: %s\nsave one with: grimoire skills accept ID\n", md)
	return 0
}

var jsonObjRE = regexp.MustCompile(`(?s)\{.*\}`)

// modelTitler asks the configured model for a title and a one-sentence
// description of a candidate, and nothing else. A reply that is not the
// expected JSON is an error, so the deterministic title stays.
func modelTitler(e *env) skillmine.Titler {
	return func(c skillmine.Candidate) (string, string, error) {
		prompt := "Name this recurring sequence of shell commands, which a developer runs often, so it can be saved as a procedure.\n" +
			"Reply with JSON only: {\"title\": \"imperative, at most 8 words\", \"description\": \"one sentence on when to use it\"}.\n\nSteps:\n"
		for i, ex := range c.Examples {
			prompt += fmt.Sprintf("%d. %s\n", i+1, ex)
		}
		if len(c.Projects) > 0 {
			prompt += "Usually run in: " + strings.Join(c.Projects, ", ") + "\n"
		}
		reply, err := e.server.AI.Complete(prompt, "")
		if err != nil {
			return "", "", err
		}
		var out struct{ Title, Description string }
		if err := json.Unmarshal([]byte(jsonObjRE.FindString(reply)), &out); err != nil {
			return "", "", fmt.Errorf("model reply was not JSON")
		}
		return out.Title, out.Description, nil
	}
}

func skillsAccept(f *bankFlags) int {
	if len(f.pos) != 1 {
		return fail("usage: grimoire skills accept ID   (ids are in the review queue)")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	_, js := queuePaths(e)
	raw, err := os.ReadFile(js)
	if err != nil {
		return fail("no review queue yet; run: grimoire skills mine")
	}
	var cands []skillmine.Candidate
	if err := json.Unmarshal(raw, &cands); err != nil {
		return fail("%s: %v", js, err)
	}
	for _, c := range cands {
		if !strings.HasPrefix(c.ID, f.pos[0]) {
			continue
		}
		store := e.storeDir(storeArgs(f))
		if err := os.MkdirAll(store, 0o755); err != nil {
			return fail("%v", err)
		}
		path := filepath.Join(store, skillmine.DraftFile(c))
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return fail("%s already exists; nothing written", path)
		}
		_, werr := fh.WriteString(skillmine.Draft(c))
		if cerr := fh.Close(); werr != nil || cerr != nil {
			return fail("could not write %s", path)
		}
		fmt.Printf("saved %s\nreview it, then: grimoire memory index --write   (and grimoire skills export --agent NAME to publish it as a skill)\n", path)
		return 0
	}
	ids := make([]string, 0, len(cands))
	for _, c := range cands {
		ids = append(ids, c.ID)
	}
	sort.Strings(ids)
	return fail("no candidate %q in the queue (have: %s)", f.pos[0], strings.Join(ids, ", "))
}
