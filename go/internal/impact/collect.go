package impact

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/agentprofile"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/skillexport"
	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// CollectOptions says what to measure.
type CollectOptions struct {
	Home  string        // for agent profiles
	Store string        // the canonical memory directory ("" = no memories)
	Since time.Duration // how far back to read sessions (default 90 days)
	Agent string        // only this agent's sessions ("" = all)
	Limit int           // rows returned, newest first (default 60)
	Now   time.Time
	Min   int // sessions per side (default 5)
	// AllSessions keeps headless, benchmark and automated prompts in the
	// re-tell series. Off by default: a re-tell needs a person who typed it.
	AllSessions bool
}

// Collect reads the sessions of every agent whose profile has transcripts,
// the memories in the store and the skills Grimoire exported, and compares.
func Collect(o CollectOptions) (Report, []error) {
	if o.Since <= 0 {
		o.Since = 90 * 24 * time.Hour
	}
	if o.Limit <= 0 {
		o.Limit = 60
	}
	if o.Now.IsZero() {
		o.Now = time.Now()
	}
	since := o.Now.Add(-o.Since)
	profiles, errs := agentprofile.All(o.Home)
	var stats []Friction
	var items []Item
	for _, p := range profiles {
		if p.Transcripts.Glob != "" && (o.Agent == "" || o.Agent == p.Name) {
			sessions, rerrs := transcript.ReadAll(transcript.Spec{Agent: p.Name, Format: p.Transcripts.Format,
				Glob: p.Transcripts.Glob, Map: p.Transcripts.Map}, transcript.Options{Since: since, MaxFiles: 3000, NoAssistantText: true})
			errs = append(errs, rerrs...)
			stats = append(stats, Stats(sessions)...)
		}
		if p.SkillsDir != "" {
			for _, m := range skillexport.ListManaged(p.SkillsDir) {
				items = append(items, Item{Kind: "skill", Name: m.Name, Agent: m.Marker.Agent, At: m.Marker.ExportedAt,
					DateSource: "exported", Terms: []string{strings.ReplaceAll(m.Name, "-", " "), strings.TrimSuffix(m.Marker.Source, ".md")}})
			}
		}
	}
	if o.Store != "" {
		if notes, err := memstore.LoadDir(o.Store); err == nil {
			for _, n := range notes {
				at, src := noteDate(n)
				items = append(items, Item{Kind: "memory", Name: n.Title, At: at, DateSource: src,
					Terms: []string{n.Title, n.Description, strings.TrimSuffix(n.File, ".md"), strings.Join(skillexport.Cues(n), " ")}})
			}
		}
	}
	// An item that landed before the window began (or after the last session)
	// has nothing to compare against.
	var lo, hi time.Time
	for _, s := range stats {
		if !s.Start.IsZero() && (lo.IsZero() || s.Start.Before(lo)) {
			lo = s.Start
		}
		if s.Start.After(hi) {
			hi = s.Start
		}
	}
	kept := items[:0]
	for _, it := range items {
		if !it.At.IsZero() && it.At.After(lo) && it.At.Before(hi) {
			kept = append(kept, it)
		}
	}
	rep := Compute(kept, stats, Options{MinPerSide: o.Min})
	rep.Since = since
	if len(rep.Rows) > o.Limit {
		rep.Rows = rep.Rows[:o.Limit]
	}
	return rep, errs
}

// noteDate is when a memory landed: a created date in its frontmatter, else the
// file's modification time (which also moves on edits, so it is labelled).
func noteDate(n memstore.Note) (time.Time, string) {
	for _, k := range []string{"metadata.created", "created", "metadata.created_at", "created_at", "metadata.date", "date"} {
		v := strings.TrimSpace(n.Fields[k])
		if v == "" {
			continue
		}
		for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04", "2006-01-02"} {
			if t, err := time.Parse(layout, v); err == nil {
				return t, "frontmatter"
			}
		}
	}
	return n.MTime, "file-mtime"
}

// Text renders a report for a terminal.
func Text(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n", r.Caveat)
	fmt.Fprintf(&b, "%d sessions since %s (agents: %s); at least %d sessions needed on each side.\n\n",
		r.Sessions, r.Since.Format("2006-01-02"), strings.Join(r.Agents, ", "), r.MinSide)
	if len(r.Rows) == 0 {
		b.WriteString("No memory or skill landed inside the window with sessions on both sides.\n")
		return b.String()
	}
	side := func(s Side) string {
		return fmt.Sprintf("n=%-3d errors %.1f  corrections %.1f  prompts %.1f  min %.0f", s.Sessions, s.MedianErrors, s.MedianCorrect, s.MedianPrompts, s.MedianMinutes)
	}
	sort.SliceStable(r.Rows, func(i, j int) bool { return r.Rows[i].LandedAt.After(r.Rows[j].LandedAt) })
	for _, row := range r.Rows {
		fmt.Fprintf(&b, "%s %s  (%s, %s)\n", row.Kind, filepath.Base(row.Name), row.LandedAt.Format("2006-01-02"), row.DateSource)
		fmt.Fprintf(&b, "  all sessions    before %s\n                  after  %s\n                  %s\n", side(row.All.Before), side(row.All.After), row.All.Verdict)
		if row.Topical != nil && (row.Topical.Before.Sessions+row.Topical.After.Sessions) > 0 {
			fmt.Fprintf(&b, "  on-topic only   before %s\n                  after  %s\n                  %s\n", side(row.Topical.Before), side(row.Topical.After), row.Topical.Verdict)
		}
	}
	b.WriteString("\n(median per session; errors = tool calls that failed; corrections = user turns that push back, a text heuristic)\n")
	return b.String()
}

// RetellCollect reads the whole transcript history (not just a recent window)
// and the memory store, and computes the weekly re-tell series.
func RetellCollect(o CollectOptions, ro RetellOptions) (RetellReport, []error) {
	profiles, errs := agentprofile.All(o.Home)
	var prompts []Prompt
	ex := newExclusions(!o.AllSessions)
	for _, p := range profiles {
		if p.Transcripts.Glob == "" || (o.Agent != "" && o.Agent != p.Name) {
			continue
		}
		opt := transcript.Options{NoAssistantText: true}
		if o.Since > 0 {
			opt.Since = o.Now.Add(-o.Since)
		}
		ss, rerrs := transcript.ReadAll(transcript.Spec{Agent: p.Name, Format: p.Transcripts.Format, Glob: p.Transcripts.Glob, Map: p.Transcripts.Map}, opt)
		errs = append(errs, rerrs...)
		ps, pe := PromptsOfInteractive(ss, !o.AllSessions)
		prompts = append(prompts, ps...)
		ex.Merge(pe)
	}
	if !o.AllSessions {
		var dup int
		prompts, dup = DedupePrompts(prompts)
		if dup > 0 {
			ex.PromptsExcluded[RuleDuplicate] += dup
			ex.PromptsKept -= dup
		}
	}
	var notes []NoteText
	if o.Store != "" {
		if ns, err := memstore.LoadDir(o.Store); err == nil {
			for _, n := range ns {
				at, _ := noteDate(n)
				notes = append(notes, NoteText{Name: n.File, Text: n.Title + "\n" + n.Description + "\n" + n.Body, At: at})
			}
		}
	}
	rep := Retells(prompts, notes, ro)
	rep.Exclusions = ex
	return rep, errs
}
