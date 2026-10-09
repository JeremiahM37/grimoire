package impact

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/transcript"
)

// Re-tells: how often a person has to say again something the agent's memory
// already held. Computed retroactively over historical transcripts so there is
// a baseline from before memory injection and a series after it.
//
// For each user prompt the best-matching memory is found by shared terms (a
// cheap pre-filter, so the judge is asked only about plausible pairs), then a
// judge says whether the prompt restates what that memory says. The judge is
// the configured re-tell judge (Jev or a local Laya, the same question the live
// path asks) or, with none configured, a strict lexical rule. Judged pairs are
// cached on disk, so a re-run costs nothing for pairs already seen.
//
// Limits, stated in every report: the pre-filter misses re-tells phrased with
// different words (the rate is a floor); a memory that was written after the
// prompt, usually because of it, counts as "held" only by now, so the report
// also gives the count where the memory already existed at the time.

// Judge answers whether prompt restates what memory says.
type Judge func(memory, prompt string) (bool, error)

// NoteText is a memory as the re-tell pass sees it.
type NoteText struct {
	Name string
	Text string // title, description and body
	At   time.Time
}

// Prompt is one user message.
type Prompt struct {
	At    time.Time
	Agent string
	Text  string
}

// PromptsOf extracts user prompts worth judging from sessions.
func PromptsOf(sessions []transcript.Session) []Prompt {
	var out []Prompt
	for _, s := range sessions {
		for _, t := range s.Turns {
			if t.Role != transcript.RoleUser || len(strings.Fields(t.Text)) < 3 {
				continue
			}
			at := t.At
			if at.IsZero() {
				at = s.Start
			}
			if at.IsZero() {
				continue
			}
			out = append(out, Prompt{At: at, Agent: s.Agent, Text: t.Text})
		}
	}
	return out
}

// RetellOptions shapes Retells.
type RetellOptions struct {
	Judge     Judge  // nil = strict lexical rule
	JudgeName string // names the judge in the cache key and the report
	Budget    int    // most NEW judge calls (default 2000)
	CachePath string // "" = no cache
	Cutoff    time.Time
	Workers   int
}

// WeekRow is one ISO week.
type WeekRow struct {
	Week         string  `json:"week"` // 2026-W41
	Prompts      int     `json:"user_prompts"`
	Candidates   int     `json:"candidates"`
	Evaluated    int     `json:"evaluated"`
	Yes          int     `json:"retells_found"`
	Retells      float64 `json:"retells_estimated"`
	PerHundred   float64 `json:"retells_per_100_prompts"`
	HeldThen     int     `json:"retells_where_memory_already_existed"`
	AfterCutoff  bool    `json:"after_cutoff"`
	SampledShare float64 `json:"evaluated_share_of_candidates"`
}

// Phase totals the weeks on one side of the cutoff.
type Phase struct {
	Weeks      int     `json:"weeks"`
	Prompts    int     `json:"user_prompts"`
	Retells    float64 `json:"retells_estimated"`
	PerHundred float64 `json:"retells_per_100_prompts"`
	HeldThen   int     `json:"retells_where_memory_already_existed"`
}

// RetellReport is the answer.
type RetellReport struct {
	Judge      string    `json:"judge"`
	Cutoff     time.Time `json:"cutoff"`
	Budget     int       `json:"budget"`
	JudgeCalls int       `json:"judge_calls_made"`
	CacheHits  int       `json:"cache_hits"`
	Errors     int       `json:"judge_errors"`
	Sampled    bool      `json:"sampled"`
	Memories   int       `json:"memories"`
	Weeks      []WeekRow `json:"weeks"`
	Before     Phase     `json:"before"`
	After      Phase     `json:"after"`
	Caveat     string    `json:"caveat"`
}

const retellCaveat = "Re-tells are found by a judge over (prompt, best-matching memory) pairs chosen by shared terms, so the rate is a floor: " +
	"a re-tell in different words is missed. A memory written because of a re-tell counts as held only 'by now'; the " +
	"'already existed' column counts re-tells where it was there at the time. Correlation only."

type cacheRec struct {
	K   string `json:"k"`
	Yes bool   `json:"y"`
}

type cand struct {
	p      Prompt
	note   int
	key    string
	hash   string
	yes    bool
	known  bool
	failed bool
}

// Retells computes the weekly re-tell rate.
func Retells(prompts []Prompt, notes []NoteText, o RetellOptions) RetellReport {
	if o.Budget <= 0 {
		o.Budget = 2000
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	name := o.JudgeName
	if o.Judge == nil {
		name = "strict"
	}
	rep := RetellReport{Judge: name, Cutoff: o.Cutoff, Budget: o.Budget, Memories: len(notes), Caveat: retellCaveat, Weeks: []WeekRow{}}
	cache := loadCache(o.CachePath)

	// Index memories by term.
	noteTerms := make([]map[string]bool, len(notes))
	inv := map[string][]int{}
	for i, n := range notes {
		noteTerms[i] = termSet(n.Text)
		for w := range noteTerms[i] {
			inv[w] = append(inv[w], i)
		}
	}
	// A week that contains the cutoff is two rows, so BEFORE and AFTER never mix.
	weeks := map[string]*WeekRow{}
	week := func(t time.Time) *WeekRow {
		y, w := t.ISOWeek()
		after := !o.Cutoff.IsZero() && !t.Before(o.Cutoff)
		k := fmt.Sprintf("%d-W%02d/%v", y, w, after)
		r := weeks[k]
		if r == nil {
			r = &WeekRow{Week: fmt.Sprintf("%d-W%02d", y, w), AfterCutoff: after}
			weeks[k] = r
		}
		return r
	}
	var cands []*cand
	for _, p := range prompts {
		week(p.At).Prompts++
		pt := termSet(p.Text)
		if len(pt) < 3 || len(p.Text) > 3000 {
			continue
		}
		hits := map[int]int{}
		for w := range pt {
			if len(inv[w]) > 40 { // a term in many memories says nothing
				continue
			}
			for _, i := range inv[w] {
				hits[i]++
			}
		}
		best, bestN := -1, 0
		for i, n := range hits {
			if n > bestN || n == bestN && best >= 0 && notes[i].Name < notes[best].Name {
				best, bestN = i, n
			}
		}
		if best < 0 || bestN < 3 || float64(bestN) < 0.3*float64(len(pt)) {
			continue
		}
		h := sha256.Sum256([]byte(p.Text))
		nh := sha256.Sum256([]byte(notes[best].Text))
		c := &cand{p: p, note: best, hash: hex.EncodeToString(h[:8])}
		c.key = name + "|" + hex.EncodeToString(nh[:8]) + "|" + c.hash
		week(p.At).Candidates++
		if y, ok := cache[c.key]; ok {
			c.yes, c.known = y, true
			rep.CacheHits++
		} else if o.Judge == nil {
			c.yes, c.known = strictRetell(pt, noteTerms[best], bestN), true
			cache[c.key] = c.yes // cheap; not persisted
		}
		cands = append(cands, c)
	}

	// New judge calls: a deterministic sample when over budget.
	var todo []*cand
	for _, c := range cands {
		if !c.known {
			todo = append(todo, c)
		}
	}
	if len(todo) > o.Budget {
		sort.Slice(todo, func(i, j int) bool { return todo[i].hash < todo[j].hash })
		todo = todo[:o.Budget]
		rep.Sampled = true
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	jobs := make(chan *cand)
	var fresh []cacheRec
	for w := 0; w < o.Workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				yes, err := o.Judge(clip(notes[c.note].Text, 1200), clip(c.p.Text, 1200))
				mu.Lock()
				rep.JudgeCalls++
				if err != nil {
					rep.Errors++
					c.failed = true
				} else {
					c.yes, c.known = yes, true
					fresh = append(fresh, cacheRec{c.key, yes})
				}
				mu.Unlock()
			}
		}()
	}
	if o.Judge != nil {
		for _, c := range todo {
			jobs <- c
		}
	}
	close(jobs)
	wg.Wait()
	saveCache(o.CachePath, fresh)

	for _, c := range cands {
		if !c.known {
			continue
		}
		r := week(c.p.At)
		r.Evaluated++
		if c.yes {
			r.Yes++
			if !notes[c.note].At.IsZero() && !notes[c.note].At.After(c.p.At) {
				r.HeldThen++
			}
		}
	}
	for _, r := range weeks {
		if r.Evaluated > 0 {
			r.SampledShare = round2(float64(r.Evaluated) / float64(r.Candidates))
			r.Retells = round1(float64(r.Yes) * float64(r.Candidates) / float64(r.Evaluated))
		}
		if r.Prompts > 0 {
			r.PerHundred = round1(100 * r.Retells / float64(r.Prompts))
		}
		rep.Weeks = append(rep.Weeks, *r)
	}
	sort.Slice(rep.Weeks, func(i, j int) bool {
		a, b := rep.Weeks[i], rep.Weeks[j]
		if a.Week != b.Week {
			return a.Week < b.Week
		}
		return !a.AfterCutoff && b.AfterCutoff
	})
	for _, r := range rep.Weeks {
		ph := &rep.Before
		if r.AfterCutoff {
			ph = &rep.After
		}
		ph.Weeks++
		ph.Prompts += r.Prompts
		ph.Retells += r.Retells
		ph.HeldThen += r.HeldThen
	}
	for _, ph := range []*Phase{&rep.Before, &rep.After} {
		if ph.Prompts > 0 {
			ph.PerHundred = round1(100 * ph.Retells / float64(ph.Prompts))
		}
		ph.Retells = round1(ph.Retells)
	}
	return rep
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

func termSet(s string) map[string]bool {
	out := map[string]bool{}
	for w := range wordSet(s) {
		if len(w) >= 4 && !retellStop[w] {
			out[w] = true
		}
	}
	return out
}

var retellStop = map[string]bool{"that": true, "this": true, "with": true, "have": true, "from": true, "your": true, "what": true,
	"when": true, "will": true, "should": true, "would": true, "could": true, "there": true, "about": true, "which": true,
	"then": true, "them": true, "they": true, "just": true, "make": true, "like": true, "into": true, "also": true, "need": true,
	"please": true, "again": true, "does": true, "dont": true, "want": true, "using": true, "file": true, "files": true}

// strictRetell is the no-judge rule: nearly all of the prompt's content terms
// are in the memory and there are enough of them to mean something.
func strictRetell(prompt, note map[string]bool, shared int) bool {
	return shared >= 5 && float64(shared) >= 0.75*float64(len(prompt))
}

func loadCache(path string) map[string]bool {
	m := map[string]bool{}
	if path == "" {
		return m
	}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<16), 1<<20)
	for sc.Scan() {
		var r cacheRec
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.K != "" {
			m[r.K] = r.Yes
		}
	}
	return m
}

func saveCache(path string, recs []cacheRec) {
	if path == "" || len(recs) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, r := range recs {
		raw, _ := json.Marshal(r)
		w.Write(append(raw, '\n'))
	}
	w.Flush()
}

// RetellText renders the weekly series for a terminal.
func RetellText(r RetellReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Re-tells per 100 user prompts, by week (judge: %s", r.Judge)
	if r.Sampled {
		fmt.Fprintf(&b, "; SAMPLED: more than %d new pairs, so a fixed sample was judged and counts are scaled", r.Budget)
	}
	fmt.Fprintf(&b, ")\n%s\n\n", r.Caveat)
	fmt.Fprintf(&b, "%-9s %8s %6s %6s %8s %9s %6s\n", "week", "prompts", "cands", "judged", "re-tells", "per 100", "held*")
	for _, w := range r.Weeks {
		mark := " "
		if w.AfterCutoff {
			mark = "+"
		}
		fmt.Fprintf(&b, "%-9s %8d %6d %6d %8.1f %9.1f %6d %s\n", w.Week, w.Prompts, w.Candidates, w.Evaluated, w.Retells, w.PerHundred, w.HeldThen, mark)
	}
	fmt.Fprintf(&b, "\nBEFORE %s: %d weeks, %d prompts, %.1f re-tells = %.1f per 100 prompts (%d where the memory already existed)\n",
		r.Cutoff.Format("2006-01-02"), r.Before.Weeks, r.Before.Prompts, r.Before.Retells, r.Before.PerHundred, r.Before.HeldThen)
	fmt.Fprintf(&b, "AFTER:  %d weeks, %d prompts, %.1f re-tells = %.1f per 100 prompts (%d where the memory already existed)\n",
		r.After.Weeks, r.After.Prompts, r.After.Retells, r.After.PerHundred, r.After.HeldThen)
	fmt.Fprintf(&b, "(%d memories; %d judge calls made, %d from cache, %d errors; + = after the cutoff; *held = memory already existed at the time)\n",
		r.Memories, r.JudgeCalls, r.CacheHits, r.Errors)
	return b.String()
}
