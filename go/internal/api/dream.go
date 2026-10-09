package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/dream/filemem"
	"github.com/JeremiahM37/grimoire/go/internal/dream/notecheck"
	"github.com/JeremiahM37/grimoire/go/internal/dream/secscan"
	"github.com/JeremiahM37/grimoire/go/internal/markdown"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Dreaming: the periodic offline pass over agent memory.
//
// A dream reads every memory system the vault holds — memory notes, memory
// banks, and file-based agent memory (a folder of notes with a MEMORY.md
// index) — runs the hygiene checks and the security sweep over them, and
// writes one report note. It changes things only when told to, and then only
// mechanically: an index line pointing at a file that is gone, a note the
// index forgot to list, a bank whose consolidation was never run. Anything
// that needs judgment — which of two duplicate facts to keep, whether a
// credential is live — is reported, never decided, because the store's whole
// promise is that a person's write is not silently overruled.

// ErrDreaming is returned when a dream is asked for while one is running.
var ErrDreaming = errors.New("a dream is already running")

// errNoChange is how a scheduled dream says it had nothing new to look at.
var errNoChange = errors.New("memory unchanged since the last dream")

// dreamState is what survives a restart: when the last dream ran and what
// memory looked like then, so the schedule can skip a dream that would only
// repeat the last report.
type dreamState struct {
	At          time.Time `json:"at"`
	Fingerprint string    `json:"fingerprint"`
}

func (s *Server) dreamStatePath() string {
	return filepath.Join(s.Vault.Root, ".grimoire", "dream-state.json")
}

func (s *Server) loadDreamState() dreamState {
	var st dreamState
	if b, err := os.ReadFile(s.dreamStatePath()); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	return st
}

func (s *Server) saveDreamState(st dreamState) {
	b, _ := json.Marshal(st)
	p := s.dreamStatePath()
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// dreamDocs collects the documents a dream reads. Raw file text is used, not
// the parsed body, so line numbers in findings are line numbers in the file a
// person opens — and so a fix can be checked against exactly what is there.
func (s *Server) dreamDocs() ([]dream.Doc, []string, error) {
	paths, err := s.Vault.Walk()
	if err != nil {
		return nil, nil, err
	}
	roots := filemem.Roots(paths)
	inRoot := func(p string) bool {
		dir := path.Dir(p)
		if dir == "." {
			dir = ""
		}
		for _, r := range roots {
			if dir == r {
				return true
			}
		}
		return false
	}
	reportDir := strings.Trim(s.dreamReportDir(), "/")
	var docs []dream.Doc
	for _, p := range paths {
		var kind dream.Kind
		switch {
		case reportDir != "" && strings.HasPrefix(p, reportDir+"/"):
			continue // the dream's own report is not memory
		case strings.HasPrefix(p, memory.Dir+"/"):
			kind = dream.KindMemoryNote
		case strings.HasPrefix(p, "banks/"):
			kind = dream.KindBank
		case inRoot(p):
			kind = dream.KindFileMemory
		default:
			continue
		}
		n, err := s.Vault.Read(p)
		if err != nil || n.Encrypted {
			continue // sealed text is already protected; unreadable is not a finding
		}
		docs = append(docs, dream.Doc{
			Path: p, Body: n.Raw, Kind: kind,
			Mtime: time.Unix(0, int64(n.MTime*1e9)),
		})
	}
	return docs, roots, nil
}

// fingerprint summarises the memory a dream would read. Equal fingerprints
// mean a scheduled dream would only repeat itself.
func fingerprint(docs []dream.Doc) string {
	h := sha256.New()
	for _, d := range docs {
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", d.Path, len(d.Body), d.Mtime.UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// knownSecrets returns the credential vault's values, so the sweep can find a
// stored secret pasted into memory verbatim — which no shape-based scanner
// can, because most real secrets have no recognisable shape. The values never
// leave this process: the sweep reports them masked.
func (s *Server) knownSecrets() []string {
	if s.Secrets == nil || !s.Secrets.IsUnlocked() {
		return nil
	}
	names, err := s.Secrets.ListNames()
	if err != nil {
		return nil
	}
	var out []string
	for _, n := range names {
		if v, err := s.Secrets.Get(n); err == nil && len(strings.TrimSpace(v)) >= 8 {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) dreamReportDir() string {
	if s.Settings == nil {
		return "Dreams"
	}
	return strings.TrimSpace(s.Settings.Get("dream_report_dir"))
}

func (s *Server) setting(key string) string {
	if s.Settings == nil {
		return ""
	}
	return strings.TrimSpace(s.Settings.Get(key))
}

// Dream runs one pass. apply allows mechanical fixes and bank consolidation;
// onlyIfChanged makes it a no-op (errNoChange) when memory is exactly as the
// last dream found it, which is what the schedule wants and a person asking
// for a dream does not.
func (s *Server) Dream(ctx context.Context, apply, onlyIfChanged bool) (*dream.Report, error) {
	if !s.dreamMu.TryLock() {
		return nil, ErrDreaming
	}
	defer s.dreamMu.Unlock()

	rep := &dream.Report{Started: time.Now().UTC()}
	docs, roots, err := s.dreamDocs()
	if err != nil {
		return nil, err
	}
	fp := fingerprint(docs)
	if onlyIfChanged && s.loadDreamState().Fingerprint == fp {
		return nil, errNoChange
	}
	rep.Docs = len(docs)

	var findings []dream.Finding
	findings = append(findings, secscan.Scan(docs, s.knownSecrets())...)
	findings = append(findings, notecheck.CheckWith(docs, time.Now(), s.freshPriors())...)
	for _, root := range roots {
		var in []dream.Doc
		for _, d := range docs {
			if d.Kind != dream.KindFileMemory {
				continue
			}
			dir := path.Dir(d.Path)
			if dir == "." {
				dir = ""
			}
			if dir == root {
				in = append(in, d)
			}
		}
		findings = append(findings, filemem.Check(root, in)...)
	}
	if pd, canon := s.setting("dream_projects_dir"), s.setting("dream_canonical_memory"); pd != "" && canon != "" {
		findings = append(findings, filemem.Fragmented(expandHome(pd), expandHome(canon))...)
	}
	findings = append(findings, s.memstoreFindings(docs)...)
	sortFindings(findings)
	rep.Findings = findings

	if apply {
		rep.Applied = s.applyDreamFixes(findings)
		rep.Actions = append(rep.Actions, s.dreamConsolidate(ctx)...)
	}
	rep.Finished = time.Now().UTC()

	if err := s.writeDreamReport(rep); err != nil {
		log.Printf("dream: writing report: %v", err)
	}
	s.dreamLast = rep
	// Fingerprint AFTER the fixes and the report, so the next scheduled dream
	// does not mistake this dream's own edits for new memory.
	if after, _, err := s.dreamDocs(); err == nil {
		fp = fingerprint(after)
	}
	s.saveDreamState(dreamState{At: rep.Finished, Fingerprint: fp})
	return rep, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

var severityRank = map[dream.Severity]int{dream.High: 0, dream.Medium: 1, dream.Low: 2, dream.Info: 3}

func sortFindings(f []dream.Finding) {
	sort.SliceStable(f, func(a, b int) bool {
		if severityRank[f[a].Severity] != severityRank[f[b].Severity] {
			return severityRank[f[a].Severity] < severityRank[f[b].Severity]
		}
		if f[a].Path != f[b].Path {
			return f[a].Path < f[b].Path
		}
		return f[a].Line < f[b].Line
	})
}

// applyDreamFixes makes the mechanical repairs. Each document is re-read and
// every fix re-checked against its current text, so a fix computed before an
// agent's write is skipped rather than applied to the wrong line. Fixes to
// one file are applied bottom-up, so deleting a line never shifts the line
// another fix refers to. The pre-fix text goes to history first, as with any
// other edit.
func (s *Server) applyDreamFixes(findings []dream.Finding) []dream.Fix {
	byPath := map[string][]dream.Fix{}
	var order []string
	for _, f := range findings {
		if f.Fix == nil {
			continue
		}
		if _, ok := byPath[f.Fix.Path]; !ok {
			order = append(order, f.Fix.Path)
		}
		byPath[f.Fix.Path] = append(byPath[f.Fix.Path], *f.Fix)
	}
	var applied []dream.Fix
	for _, rel := range order {
		abs, err := s.Vault.SafePath(rel)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		next, done := applyFixes(string(raw), byPath[rel])
		if len(done) == 0 || next == string(raw) {
			continue
		}
		if s.History != nil {
			s.History.Snapshot(rel, string(raw))
		}
		info, _ := os.Stat(abs)
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode().Perm()
		}
		if err := os.WriteFile(abs, []byte(next), mode); err != nil {
			log.Printf("dream: fixing %s: %v", rel, err)
			continue
		}
		// A memory note is indexed fact by fact, and recall reads the index;
		// re-derive it now rather than waiting for the watcher to notice.
		if strings.HasPrefix(rel, memory.Dir+"/") && s.Index != nil {
			if _, err := s.Index.Upsert(rel); err != nil {
				log.Printf("dream: reindexing %s: %v", rel, err)
			}
		}
		applied = append(applied, done...)
	}
	return applied
}

// applyFixes applies fixes to one document's text and returns the result and
// the fixes that held. Pure, so it is tested directly.
func applyFixes(text string, fixes []dream.Fix) (string, []dream.Fix) {
	trailingNL := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	var replaces, appends []dream.Fix
	for _, f := range fixes {
		switch f.Kind {
		case dream.FixReplaceLine:
			replaces = append(replaces, f)
		case dream.FixAppend:
			appends = append(appends, f)
		}
	}
	sort.SliceStable(replaces, func(a, b int) bool { return replaces[a].Line > replaces[b].Line })
	var done []dream.Fix
	seen := map[int]bool{}
	for _, f := range replaces {
		i := f.Line - 1
		if i < 0 || i >= len(lines) || seen[i] || lines[i] != f.Old {
			continue // the document moved on since the finding; leave it
		}
		seen[i] = true
		if f.New == "" {
			lines = append(lines[:i], lines[i+1:]...)
		} else {
			lines[i] = f.New
		}
		done = append(done, f)
	}
	present := map[string]bool{}
	for _, l := range lines {
		present[l] = true
	}
	for _, f := range appends {
		if f.New == "" || present[f.New] {
			continue
		}
		lines = append(lines, f.New)
		present[f.New] = true
		done = append(done, f)
	}
	out := strings.Join(lines, "\n")
	if trailingNL || len(appends) > 0 {
		out += "\n"
	}
	return out, done
}

// dreamConsolidate queues consolidation for every bank that wants it and has
// facts waiting — the retain-time trigger is best-effort, and a dream is the
// backstop that notices when it was missed.
func (s *Server) dreamConsolidate(_ context.Context) []string {
	if s.Banks == nil {
		return nil
	}
	banks, err := s.Banks.ListBanks()
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range banks {
		n := s.Banks.ConsolidationDue(b.ID)
		if n == 0 {
			continue
		}
		if _, _, err := s.Banks.EnqueueConsolidation(b.ID); err != nil {
			out = append(out, fmt.Sprintf("could not queue consolidation for bank %s: %v", b.ID, err))
			continue
		}
		out = append(out, fmt.Sprintf("queued consolidation of %d fact(s) in bank %s", n, b.ID))
	}
	return out
}

// writeDreamReport writes the report as one note that each dream replaces, so
// a vault gains a single page rather than a page a night. The previous report
// goes to history like any other overwritten note.
func (s *Server) writeDreamReport(rep *dream.Report) error {
	dir := s.dreamReportDir()
	if dir == "" {
		return nil // reporting to the vault is off; the API still has it
	}
	rel := strings.Trim(dir, "/") + "/Dream report.md"
	if prev, err := s.Vault.Read(rel); err == nil && s.History != nil {
		s.History.Snapshot(rel, prev.Body)
	}
	fm := markdown.NewFrontmatter()
	fm.Set("title", "Dream report")
	fm.Set("dream", true)
	fm.Set("tags", []string{"dream"})
	_, err := s.Vault.Write(rel, renderDreamReport(rep), fm)
	return err
}

// renderDreamReport is the human page: most severe first, each finding linked
// to its note, nothing that could be a credential.
func renderDreamReport(rep *dream.Report) string {
	var b strings.Builder
	counts := map[dream.Severity]int{}
	for _, f := range rep.Findings {
		counts[f.Severity]++
	}
	fmt.Fprintf(&b, "# Dream report\n\n")
	fmt.Fprintf(&b, "Dreamt %s over %d memory documents: **%d high**, %d medium, %d low, %d info.\n\n",
		rep.Finished.Local().Format("2006-01-02 15:04"), rep.Docs,
		counts[dream.High], counts[dream.Medium], counts[dream.Low], counts[dream.Info])
	if len(rep.Applied) > 0 || len(rep.Actions) > 0 {
		b.WriteString("## Done\n\n")
		for _, f := range rep.Applied {
			switch {
			case f.Kind == dream.FixAppend:
				fmt.Fprintf(&b, "- Added to [[%s]]: `%s`\n", strings.TrimSuffix(f.Path, ".md"), clip(f.New, 100))
			case f.New == "":
				fmt.Fprintf(&b, "- Removed from [[%s]] line %d: `%s`\n", strings.TrimSuffix(f.Path, ".md"), f.Line, clip(f.Old, 100))
			default:
				fmt.Fprintf(&b, "- Rewrote [[%s]] line %d\n", strings.TrimSuffix(f.Path, ".md"), f.Line)
			}
		}
		for _, a := range rep.Actions {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	for _, cat := range []dream.Category{dream.Security, dream.Hygiene} {
		var fs []dream.Finding
		for _, f := range rep.Findings {
			if f.Category == cat {
				fs = append(fs, f)
			}
		}
		if len(fs) == 0 {
			continue
		}
		title := map[dream.Category]string{dream.Security: "Security", dream.Hygiene: "Hygiene"}[cat]
		fmt.Fprintf(&b, "## %s (%d)\n\n", title, len(fs))
		for _, f := range fs {
			loc := strings.TrimSuffix(f.Path, ".md")
			where := "[[" + loc + "]]"
			if strings.HasPrefix(f.Path, "/") {
				where = "`" + f.Path + "`" // outside the vault
			}
			if f.Line > 0 {
				where += " line " + strconv.Itoa(f.Line)
			}
			fmt.Fprintf(&b, "- **%s** `%s` %s — %s", f.Severity, f.Check, where, f.Message)
			if f.Excerpt != "" {
				fmt.Fprintf(&b, " (`%s`)", clip(strings.ReplaceAll(f.Excerpt, "`", "'"), 120))
			}
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(rep.Findings) == 0 {
		b.WriteString("Nothing to report.\n")
	}
	return b.String()
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// ---------------------------------------------------------------- routes

func (s *Server) dreamNow(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Apply bool `json:"apply"`
	}
	if r.ContentLength > 0 && !decodeJSON(w, r, &body) {
		return
	}
	rep, err := s.Dream(r.Context(), body.Apply, false)
	if errors.Is(err, ErrDreaming) {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) dreamReport(w http.ResponseWriter, _ *http.Request) {
	s.dreamMu.Lock()
	rep := s.dreamLast
	s.dreamMu.Unlock()
	st := s.loadDreamState()
	writeJSON(w, http.StatusOK, map[string]any{"last": rep, "last_at": st.At})
}

// DreamLoop runs scheduled dreams until done is closed. It wakes hourly and
// dreams when the configured interval has passed AND memory changed since the
// last dream: a quiet vault costs one directory walk an hour and no writes.
func (s *Server) DreamLoop(done <-chan struct{}) {
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	check := func() {
		hours, err := strconv.ParseFloat(s.setting("dream_interval_hours"), 64)
		if err != nil || hours <= 0 {
			return
		}
		if time.Since(s.loadDreamState().At) < time.Duration(hours*float64(time.Hour)) {
			return
		}
		apply := s.setting("dream_apply") != "off"
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
		defer cancel()
		rep, err := s.Dream(ctx, apply, true)
		switch {
		case errors.Is(err, errNoChange), errors.Is(err, ErrDreaming):
		case err != nil:
			log.Printf("dream: %v", err)
		default:
			log.Printf("dream: %d documents, %d findings, %d fixes", rep.Docs, len(rep.Findings), len(rep.Applied))
		}
	}
	// first look shortly after start, not an hour in
	select {
	case <-done:
		return
	case <-time.After(2 * time.Minute):
		check()
	}
	for {
		select {
		case <-done:
			return
		case <-tick.C:
			check()
		}
	}
}
