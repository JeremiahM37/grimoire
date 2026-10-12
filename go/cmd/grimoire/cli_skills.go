package main

import (
	"encoding/json"
	"errors"
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
	"github.com/JeremiahM37/grimoire/go/internal/skillgate"
	"github.com/JeremiahM37/grimoire/go/internal/skillmine"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

const skillsUsage = `usage:
  grimoire skills export --agent NAME [--dry-run] [--remove] [--force] [--dest DIR] [--dir STORE]
  grimoire skills mine [--agent NAME]... [--since 30d] [--min-sessions 3] [--limit 20] [--max-files 2000] [--title] [--dry-run]
  grimoire skills gate ID
  grimoire skills accept ID [--override "REASON"] [--dir STORE]
  grimoire skills check [--cwd DIR] [--dir STORE] [--json]
  grimoire skills write --file FILE.json [--json]

export writes each procedure memory in the store as a skill folder (SKILL.md with a
name and a description built from the memory's cues, plus scripts/ when it holds shell
blocks) into the agent's skills dir (the profile's skills_dir, or --dest). A skill it
did not write is never touched; one it wrote and you edited is skipped unless --force;
--remove deletes only what it wrote.

mine reads the agents' session transcripts (profiles with a transcripts section), finds
command sequences that recur across at least --min-sessions sessions, and writes drafts
to a review queue in the vault's .grimoire/ folder. Nothing becomes a memory until you
run accept. --title lets the configured model title a candidate; it decides nothing else.
Every candidate is also gated offline against the same sessions (support, trigger
false fires, failed outcomes). accept refuses a failed gate unless you give
--override with the reason, which is recorded in the skill's frontmatter.

gate prints a candidate's verdict and the numbers behind it. check lists the stored
skills whose preconditions hold in the working directory, and which do not, and why.
A precondition written as an anchor (anchor:...) holds when its check passes here.
--json prints the same report as JSON.

write saves a procedure the agent just worked out: FILE.json holds {name, when, steps,
pre, post, host?, topic?, agent?, session?}, where pre and post are anchor objects.
The store runs every pre check once and refuses to save when one does not hold (exit 1,
nothing written). The procedure is saved into the server's memory store
(memory_canonical_dir) and can be re-checked with check and published with export.`

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
	case "gate":
		return skillsGate(f)
	case "check":
		return skillsCheck(f)
	case "write":
		return skillsWrite(f)
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
	skillgate.Apply(cands, sessions, skillgate.Options{})

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
		fmt.Printf("  %s  %2d sessions  %-40s  %s\n", c.ID, c.Sessions, c.Title, gateSummary(c.Gate))
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
		if c.Gate == nil {
			return fail("%s has no gate result (queued before the gate existed); run: grimoire skills mine", c.ID)
		}
		override := f.str("--override", "")
		if !c.Gate.Pass {
			if override == "" {
				return fail("%s failed the offline gate:\n%s\naccept it anyway with --override \"reason\"", c.ID, reasonLines(c.Gate))
			}
			c.Gate.Override = override
		}
		stamped, err := skillgate.Stamp(skillmine.Draft(c), skillgate.FromCandidate(c), c.Gate)
		if err != nil {
			return fail("%v", err)
		}
		store := e.storeDir(storeArgs(f))
		if err := os.MkdirAll(store, 0o755); err != nil {
			return fail("%v", err)
		}
		path, err := skillgate.SaveNote(store, skillmine.DraftFile(c), stamped, false)
		if err != nil {
			return fail("%v", err)
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

func gateSummary(g *skillmine.Gate) string {
	switch {
	case g == nil:
		return "gate: not run"
	case g.Pass:
		return fmt.Sprintf("gate: pass (%d sessions, %d/%d false fires)", g.Support, g.HeldOutFires, g.HeldOut)
	case g.Override != "":
		return "gate: FAILED, overridden"
	}
	return "gate: FAILED (" + strings.Join(g.Reasons, "; ") + ")"
}

func reasonLines(g *skillmine.Gate) string {
	var b strings.Builder
	for _, r := range g.Reasons {
		b.WriteString("  - " + r + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func skillsGate(f *bankFlags) int {
	if len(f.pos) != 1 {
		return fail("usage: grimoire skills gate ID   (ids are in the review queue)")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	c, ok := queuedCandidate(e, f.pos[0])
	if !ok {
		return fail("no candidate %q in the queue; run: grimoire skills mine", f.pos[0])
	}
	g := c.Gate
	if g == nil {
		return fail("%s has no gate result; run: grimoire skills mine", c.ID)
	}
	fmt.Printf("%s  %s\n", c.ID, c.Title)
	state := "FAILED"
	if g.Pass {
		state = "pass"
	}
	fmt.Printf("gate:            %s\n", state)
	fmt.Printf("trigger:         %q\n", g.Trigger)
	fmt.Printf("support:         %d independent sessions contain the pattern\n", g.Support)
	fmt.Printf("failed outcomes: %d of %d sessions with a recorded outcome\n", g.FailedSources, g.KnownSources)
	fmt.Printf("held out:        %d sessions without the pattern; trigger fired on %d (%.0f%%)\n", g.HeldOut, g.HeldOutFires, 100*g.FalseFireRate)
	for _, r := range g.Reasons {
		fmt.Println("  - " + r)
	}
	if g.Override != "" {
		fmt.Printf("override:        %s\n", g.Override)
	}
	fmt.Println("(a statistical check over recorded sessions; it does not show the skill works)")
	if !g.Pass && g.Override == "" {
		return 1
	}
	return 0
}

func queuedCandidate(e *env, prefix string) (skillmine.Candidate, bool) {
	_, js := queuePaths(e)
	raw, err := os.ReadFile(js)
	if err != nil {
		return skillmine.Candidate{}, false
	}
	var cands []skillmine.Candidate
	if json.Unmarshal(raw, &cands) != nil {
		return skillmine.Candidate{}, false
	}
	for _, c := range cands {
		if strings.HasPrefix(c.ID, prefix) {
			return c, true
		}
	}
	return skillmine.Candidate{}, false
}

// skillsCheck lists stored skills and whether their preconditions hold here.
// Notes without a spec are ordinary procedures and are only counted.
func skillsCheck(f *bankFlags) int {
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
	cwd := f.str("--cwd", "")
	if cwd == "" {
		if cwd, err = os.Getwd(); err != nil {
			return fail("%v", err)
		}
	}
	ctx := skillgate.NewContext(cwd, nil)
	plain := 0
	type row struct {
		Title       string   `json:"title"`
		File        string   `json:"file"`
		Applies     bool     `json:"applies"`
		Unmet       []string `json:"unmet"`
		NeedsValues []string `json:"needs_values"`
	}
	rows := []row{}
	for _, n := range notes {
		s, ok := skillgate.SpecOf(n)
		if !ok {
			plain++
			continue
		}
		applies, unmet := skillgate.Applicable(s, ctx)
		rows = append(rows, row{Title: n.Title, File: n.File, Applies: applies,
			Unmet: nonNil(unmet), NeedsValues: nonNil(skillgate.NeedsValues(s))})
	}
	if f.on["--json"] {
		raw, _ := json.MarshalIndent(map[string]any{"cwd": cwd, "skills": rows, "without_spec": plain}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	for _, r := range rows {
		if r.Applies {
			fmt.Printf("applicable  %s\n", r.Title)
		} else {
			fmt.Printf("unmet       %s  (%s)\n", r.Title, strings.Join(r.Unmet, ", "))
		}
		for _, nv := range r.NeedsValues {
			fmt.Printf("            needs values: %s\n", nv)
		}
	}
	fmt.Printf("%d procedure(s) with no spec, not checked; cwd %s\n", plain, cwd)
	return 0
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// skillsWrite posts a procedure to the server, as the local agent. The server
// does the checking and the saving, so the answer is the server's.
func skillsWrite(f *bankFlags) int {
	file := f.str("--file", "")
	if file == "" {
		return fail("usage: grimoire skills write --file FILE.json [--json]")
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return fail("skills write: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil || body == nil {
		return fail("skills write: %s must be a JSON object (see grimoire skills help)", file)
	}
	// The running server writes the procedure when one answers, so the vault is
	// opened here only when none does (see postLive).
	status, resp, err := postLive("/api/memory/procedure", body)
	live := err == nil
	if errors.Is(err, errNoServer) {
		e, oerr := openEnv()
		if oerr != nil {
			return fail("%v", oerr)
		}
		defer e.close()
		status, resp = e.callFrom("127.0.0.1:0", "POST", "/api/memory/procedure", body)
	} else if err != nil {
		return fail("skills write: %v", err)
	}
	if f.on["--json"] {
		reportPath(true, live)
		fmt.Println(resp)
		if status >= 300 {
			return 1
		}
		return 0
	}
	reportPath(false, live)
	var out struct {
		Stored   bool   `json:"stored"`
		Replaced bool   `json:"replaced"`
		File     string `json:"file"`
		Detail   string `json:"detail"`
		Checks   []struct {
			Phase   string `json:"phase"`
			Type    string `json:"type"`
			Result  string `json:"result"`
			Message string `json:"message"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(resp), &out); err != nil {
		return fail("skills write: unreadable response (status %d)", status)
	}
	for _, c := range out.Checks {
		line := fmt.Sprintf("  %-4s %-16s %s", c.Phase, c.Type, c.Result)
		if c.Message != "" {
			line += "  " + c.Message
		}
		fmt.Println(line)
	}
	if status >= 300 {
		fmt.Fprintf(os.Stderr, "not stored: %s\n", out.Detail)
		return 1
	}
	verb := "saved"
	if out.Replaced {
		verb = "replaced"
	}
	fmt.Printf("%s %s\n", verb, out.File)
	return 0
}
