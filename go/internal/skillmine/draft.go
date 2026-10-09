package skillmine

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// DraftFile is the file name a candidate would be saved under.
func DraftFile(c Candidate) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(c.Title), "-"), "-")
	if len(slug) > 48 {
		slug = strings.Trim(slug[:48], "-")
	}
	if slug == "" {
		slug = "sequence"
	}
	return "procedure_" + slug + "-" + c.ID[:4] + ".md"
}

// Draft renders a candidate as the text of a procedure memory note: kind
// procedure, steps numbered, the evidence stated. The note says it is mined,
// so a reader weighs it as the record of what was done, not as advice.
func Draft(c Candidate) string {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + yamlQuote(c.Title) + "\n")
	b.WriteString("description: " + yamlQuote(c.Description) + "\n")
	b.WriteString("metadata:\n  kind: procedure\n  origin: skills-mine\n  mined_sessions: " + fmt.Sprint(c.Sessions) + "\n")
	if len(c.Projects) > 0 && c.Projects[0] != "" {
		b.WriteString("  cues: " + yamlQuote("working in "+strings.Join(c.Projects, "; or working in ")) + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("# " + c.Title + "\n\n")
	b.WriteString("Steps, as run (edit before relying on them):\n\n")
	for i, ex := range c.Examples {
		fmt.Fprintf(&b, "%d. `%s`\n", i+1, strings.ReplaceAll(oneLine(ex, 200), "`", "'"))
	}
	b.WriteString("\n" + evidence(c) + "\n")
	return b.String()
}

func evidence(c Candidate) string {
	var agents []string
	for a, n := range c.Agents {
		agents = append(agents, fmt.Sprintf("%s %d", a, n))
	}
	sort.Strings(agents)
	s := fmt.Sprintf("Mined from %d sessions (%s)", c.Sessions, strings.Join(agents, ", "))
	if !c.First.IsZero() {
		s += fmt.Sprintf(", %s to %s", c.First.Format("2006-01-02"), c.Last.Format("2006-01-02"))
	}
	return s + ". This is a record of a recurring sequence, not a verified procedure."
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		s = string(r[:max-1]) + "…"
	}
	return s
}

// Queue renders candidates as one markdown note for review: each is a
// ready-to-save procedure note in a fenced block, with its id for
// `grimoire skills accept`.
func Queue(cs []Candidate, since time.Duration, generated time.Time) string {
	var b strings.Builder
	b.WriteString("# Skill candidates (review queue)\n\n")
	fmt.Fprintf(&b, "Generated %s from the last %s of agent sessions. These are drafts: nothing here is in memory yet. "+
		"Accept one with `grimoire skills accept ID`, or edit the draft first and save it into the memory store yourself. "+
		"Delete this file when done; it is rewritten on each run.\n\n", generated.Format("2006-01-02 15:04"), humanDays(since))
	if len(cs) == 0 {
		b.WriteString("No command sequence recurred often enough.\n")
		return b.String()
	}
	for _, c := range cs {
		model := ""
		if c.Titled {
			model = " (title and description written by the configured model)"
		}
		fmt.Fprintf(&b, "## %s\n\nid `%s` · %d sessions · %d steps%s\n\n", c.Title, c.ID, c.Sessions, len(c.Units), model)
		b.WriteString("````markdown\n" + Draft(c) + "````\n\n")
	}
	return b.String()
}

func humanDays(d time.Duration) string {
	if d <= 0 {
		return "available history"
	}
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func yamlQuote(s string) string {
	if s == "" || strings.ContainsAny(s, ":#{}[],&*!|>'\"%@`") || strings.HasPrefix(s, "-") || strings.TrimSpace(s) != s {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}
