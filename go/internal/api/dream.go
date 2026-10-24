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
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
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
	// Keys are the findings the last dream reported, so the next one can say
	// which of its findings are new.
	Keys []string `json:"keys,omitempty"`
	// Asks are the last dream's questions for the owner (inbox.go).
	Asks []dream.Ask `json:"asks,omitempty"`
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
		// A dream reads the memory it is asked to check, but never a fact that
		// is private or sensitive: its findings are written into a report note,
		// and an excerpt of a hidden fact would publish it there.
		docs = append(docs, dream.Doc{
			Path: p, Body: memory.DropHidden(n.Raw), Kind: kind,
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
	info, err := s.Secrets.Describe()
	if err != nil {
		return nil
	}
	var out []string
	for _, i := range info {
		if strings.HasPrefix(i.Note, secrets.ConfigNotePrefix) {
			continue // stored beside a key, not a secret itself
		}
		if v, err := s.Secrets.Get(i.Name); err == nil && len(strings.TrimSpace(v)) >= 8 {
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
	findings = append(findings, secscan.ScanGuarded(docs, s.knownSecrets(), s.guardClassifier())...)
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
		found := filemem.Check(root, in)
		// An index past the load limit is repaired, not just reported: the
		// tiering plan rides on the truncation finding as its fix.
		if tier := filemem.Tier(root, in, s.memoryUse(root)); len(tier) > 0 {
			for i := range found {
				if found[i].Check == "index_truncated" {
					found[i].Fix, found[i].Fixes = &tier[0], tier[1:]
				}
			}
		}
		findings = append(findings, found...)
	}
	if pd, canon := s.setting("dream_projects_dir"), s.setting("dream_canonical_memory"); pd != "" && canon != "" {
		findings = append(findings, filemem.Fragmented(expandHome(pd), expandHome(canon))...)
	}
	findings = append(findings, s.memstoreFindings(docs)...)
	sortFindings(findings)
	rep.Findings = findings

	var verdicts []challengeVerdict
	if apply {
		rep.Applied, rep.Held = s.applyDreamFixesGated(findings)
		rep.Actions = append(rep.Actions, s.dreamConsolidate(ctx)...)
		verdicts = s.settleChallenges(ctx, docs, true)
	} else {
		verdicts = s.settleChallenges(ctx, docs, false)
	}
	rep.Findings = resolvedDropped(rep.Findings, rep.Applied, verdicts, apply)
	for _, v := range verdicts {
		if v.Action == "ask" {
			rep.Asks = append(rep.Asks, dream.Ask{Check: "open_challenge", Path: v.Note,
				Message: challengeLine(v),
				Action:  "Decide which fact is right in the console's Disputes panel, or run `grimoire memory challenges`."})
			continue
		}
		if apply {
			rep.Settled = append(rep.Settled, challengeLine(v))
		}
	}
	rep.Asks = append(rep.Asks, ownerAsks(rep.Findings)...)
	prev := map[string]bool{}
	for _, k := range s.loadDreamState().Keys {
		prev[k] = true
	}
	rep.New = map[string]bool{}
	keys := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		keys = append(keys, f.Key())
		if !prev[f.Key()] {
			rep.New[f.Key()] = true
		}
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
	s.saveDreamState(dreamState{At: rep.Finished, Fingerprint: fp, Keys: keys, Asks: rep.Asks})
	return rep, nil
}

// memoryUse counts how often each note directly inside root was injected
// into an agent's context in the last month, by file name. Tiering keeps the
// used ones in the loaded core. Counts only, whatever the outcome.
func (s *Server) memoryUse(root string) map[string]int {
	out := map[string]int{}
	st := s.adh()
	if st == nil {
		return out
	}
	by, _, err := st.Report(time.Now().Add(-30 * 24 * time.Hour))
	if err != nil {
		return out
	}
	for _, stats := range by {
		p := strings.TrimSuffix(stats.Path, "#")
		if i := strings.IndexByte(p, '#'); i >= 0 {
			p = p[:i]
		}
		if dir := path.Dir(p); dir == root || (root == "" && dir == ".") {
			out[path.Base(p)] += stats.Injected
		}
	}
	return out
}

// resolvedDropped removes the findings this dream has already dealt with: a
// finding whose fixes were all applied is fixed, and an open challenge the
// dream ruled on is no longer open. The report lists those under what was
// done, so leaving them among the problems would report each one twice.
func resolvedDropped(fs []dream.Finding, applied []dream.Fix, verdicts []challengeVerdict, apply bool) []dream.Finding {
	done := map[string]bool{}
	for _, f := range applied {
		done[fixKey(f)] = true
	}
	settled := map[string]bool{}
	if apply {
		for _, v := range verdicts {
			if v.Action != "ask" {
				settled[v.Note+"\x00"+v.ID] = true
			}
		}
	}
	out := fs[:0:0]
	for _, f := range fs {
		fixes := f.AllFixes()
		if len(fixes) > 0 {
			all := true
			for _, fx := range fixes {
				all = all && done[fixKey(fx)]
			}
			if all {
				continue
			}
		}
		if f.Check == "open_challenge" && len(f.Related) > 0 && challengeSettled(f, verdicts, settled) {
			continue
		}
		out = append(out, f)
	}
	return out
}

func fixKey(f dream.Fix) string {
	return f.Kind + "\x00" + f.Path + "\x00" + strconv.Itoa(f.Line) + "\x00" + f.Old + "\x00" + f.New
}

func challengeSettled(f dream.Finding, verdicts []challengeVerdict, settled map[string]bool) bool {
	for _, v := range verdicts {
		if v.Note == f.Path && v.Contested == f.Related[0] && settled[v.Note+"\x00"+v.ID] {
			return true
		}
	}
	return false
}

// ownerAsks picks the findings only a person can act on. It is a short list
// on purpose: a credential sitting in a note, text from an untrusted source
// that tries to direct an agent, invisible characters, memory written where no
// session reads it, and an index repair that could not be made. Everything
// else is either fixed by the dream or worth knowing but not worth asking.
func ownerAsks(fs []dream.Finding) []dream.Ask {
	var out []dream.Ask
	byCheck := map[string]int{}
	for _, f := range fs {
		var action string
		switch {
		case f.Check == "secret" || f.Check == "secret_value":
			action = "Store the value in the credential vault (`grimoire secret set NAME`), then delete it from the note."
		case f.Check == "injection" && f.Severity == dream.High:
			action = "Read the line. If it is not yours, delete it, or mark the fact untrusted with `grimoire memory annotate`."
		case f.Check == "untrusted_instruction":
			action = "Text other people can write became a rule an agent would follow. Delete the fact unless you agree with it."
		case f.Check == "hidden_unicode" && f.Severity == dream.High:
			action = "Open the note and remove the invisible characters on that line."
		case f.Check == "fragmented_store":
			action = "Move these files into the canonical memory directory and replace the folder with a symlink to it."
		case f.Check == "index_truncated":
			action = "The dream could not split MEMORY.md (it changed while the dream ran, or a write failed); the next dream retries."
		default:
			continue
		}
		out = append(out, dream.Ask{Check: f.Check, Path: f.Path, Message: f.Message, Action: action})
		byCheck[f.Check]++
	}
	return groupAsks(out, byCheck)
}

// groupAsks makes one question of many findings of the same kind: ninety
// copies of "a known secret is in this note" is a list nobody reads, while
// "in 90 notes, such as …" is one decision.
func groupAsks(asks []dream.Ask, byCheck map[string]int) []dream.Ask {
	var out []dream.Ask
	done := map[string]bool{}
	for _, a := range asks {
		if byCheck[a.Check] <= 1 {
			out = append(out, a)
			continue
		}
		if done[a.Check] {
			continue
		}
		done[a.Check] = true
		var paths []string
		seen := map[string]bool{}
		for _, b := range asks {
			if b.Check == a.Check && !seen[b.Path] && len(paths) < 5 {
				seen[b.Path] = true
				paths = append(paths, "[["+strings.TrimSuffix(b.Path, ".md")+"]]")
			}
		}
		out = append(out, dream.Ask{Check: a.Check,
			Message: fmt.Sprintf("%s (%d findings, in %s and more)", strings.TrimSuffix(a.Message, "."), byCheck[a.Check], strings.Join(paths, ", ")),
			Action:  a.Action})
	}
	return out
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

// applyDreamFixesGated makes the mechanical repairs. Each document is re-read and
// every fix re-checked against its current text, so a fix computed before an
// agent's write is skipped rather than applied to the wrong line. Fixes to
// one file are applied bottom-up, so deleting a line never shifts the line
// another fix refers to. The pre-fix text goes to history first, as with any
// other edit.
//
// Memory replay runs in front of every document: a document whose fixes would stop a useful memory from firing in
// past situations is left as it is and reported as held. Each document is
// judged against the store as the earlier ones left it.
func (s *Server) applyDreamFixesGated(findings []dream.Finding) ([]dream.Fix, []dream.Held) {
	gater := s.newNoteGater()
	var held []dream.Held
	byPath := map[string][]dream.Fix{}
	var order []string
	for _, f := range findings {
		for _, fx := range f.AllFixes() {
			if _, ok := byPath[fx.Path]; !ok {
				order = append(order, fx.Path)
			}
			byPath[fx.Path] = append(byPath[fx.Path], fx)
		}
	}
	var applied []dream.Fix
	for _, rel := range order {
		abs, err := s.Vault.SafePath(rel)
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(abs)
		if errors.Is(err, os.ErrNotExist) && createsFile(byPath[rel]) {
			raw, err = nil, nil // a new index part
		}
		if err != nil {
			continue
		}
		if fx, ok := removal(byPath[rel]); ok {
			if string(raw) != fx.Old {
				continue // changed since the dream read it
			}
			if s.History != nil {
				s.History.Snapshot(rel, string(raw))
			}
			if err := os.Remove(abs); err != nil {
				log.Printf("dream: removing %s: %v", rel, err)
				continue
			}
			applied = append(applied, fx)
			continue
		}
		next, done := applyFixes(string(raw), byPath[rel])
		if len(done) == 0 || next == string(raw) {
			continue
		}
		// An index (MEMORY.md and its parts) only points at memories. Its
		// fixes drop dead links and move lines between its own files, so no
		// memory can stop firing and replay has nothing to judge.
		if isIndexFile(rel) {
			if err := s.writeDreamFix(rel, abs, raw, next); err == nil {
				applied = append(applied, done...)
			}
			continue
		}
		gate, hold, afterApply := gater.check(rel, vault.NoteFromText(rel, next, 0).Body)
		if hold {
			h := dream.Held{Path: rel, Reason: gate.Verdict.Reason, Fixes: len(done)}
			for _, d := range gate.Report.Diffs {
				if len(d.Lost) > 0 && len(h.Lost) < 5 {
					h.Lost = append(h.Lost, clip(d.Text, 120))
				}
			}
			held = append(held, h)
			continue
		}
		if err := s.writeDreamFix(rel, abs, raw, next); err != nil {
			continue
		}
		// A memory note is indexed fact by fact, and recall reads the index;
		// re-derive it now rather than waiting for the watcher to notice.
		if strings.HasPrefix(rel, memory.Dir+"/") && s.Index != nil {
			if _, err := s.Index.Upsert(rel); err != nil {
				log.Printf("dream: reindexing %s: %v", rel, err)
			}
		}
		afterApply()
		applied = append(applied, done...)
	}
	return applied, held
}

// writeDreamFix writes one fixed document, history first.
func (s *Server) writeDreamFix(rel, abs string, raw []byte, next string) error {
	if s.History != nil && raw != nil {
		s.History.Snapshot(rel, string(raw))
	}
	info, _ := os.Stat(abs)
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(abs, []byte(next), mode); err != nil {
		log.Printf("dream: fixing %s: %v", rel, err)
		return err
	}
	return nil
}

func isIndexFile(rel string) bool {
	name := path.Base(rel)
	return name == "MEMORY.md" || filemem.IsIndexPart(name)
}

func createsFile(fixes []dream.Fix) bool {
	for _, f := range fixes {
		if f.Kind == dream.FixWriteFile && f.Old == "" {
			return true
		}
	}
	return false
}

func removal(fixes []dream.Fix) (dream.Fix, bool) {
	for _, f := range fixes {
		if f.Kind == dream.FixRemoveFile {
			return f, true
		}
	}
	return dream.Fix{}, false
}

// applyFixes applies fixes to one document's text and returns the result and
// the fixes that held. Pure, so it is tested directly.
func applyFixes(text string, fixes []dream.Fix) (string, []dream.Fix) {
	trailingNL := strings.HasSuffix(text, "\n")
	lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
	if text == "" {
		lines = nil
	}
	// A whole-file rewrite stands alone: it replaces the text it was planned
	// against, and line fixes computed against that text no longer apply.
	for _, f := range fixes {
		if f.Kind == dream.FixWriteFile {
			if text != f.Old {
				return text, nil
			}
			return f.New, []dream.Fix{f}
		}
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

// renderDreamReport is the human page. It answers three questions in order:
// does anything need me, what did the dream change, and what is it watching.
// The full finding list is at the bottom for whoever wants it, with what is
// new since the last dream marked, so a report that says the same thing every
// day can be read in a glance.
func renderDreamReport(rep *dream.Report) string {
	var b strings.Builder
	link := func(p string) string {
		if strings.HasPrefix(p, "/") {
			return "`" + p + "`" // outside the vault
		}
		return "[[" + strings.TrimSuffix(p, ".md") + "]]"
	}
	fmt.Fprintf(&b, "# Dream report\n\n")
	fmt.Fprintf(&b, "Dreamt %s over %d memory documents.\n\n",
		rep.Finished.Local().Format("2006-01-02 15:04"), rep.Docs)

	if len(rep.Asks) == 0 {
		b.WriteString("**Nothing needs you.**\n\n")
	} else {
		fmt.Fprintf(&b, "## Needs you (%d)\n\n", len(rep.Asks))
		for _, a := range rep.Asks {
			where := ""
			if a.Path != "" {
				where = " in " + link(a.Path)
			}
			fmt.Fprintf(&b, "- %s%s. **Do:** %s\n", strings.TrimSuffix(a.Message, "."), where, a.Action)
		}
		b.WriteString("\n")
	}

	done := len(rep.Actions) + len(rep.Settled)
	split := false
	for _, f := range rep.Applied {
		if f.Kind != dream.FixWriteFile {
			done++
		} else if !split {
			split = true
			done++ // an index and its parts are one repair
		}
	}
	if done > 0 {
		fmt.Fprintf(&b, "## Fixed (%d)\n\n", done)
		var rewrote []string
		for _, f := range rep.Applied {
			switch f.Kind {
			case dream.FixAppend:
				fmt.Fprintf(&b, "- Listed %s in its index\n", clip(f.New, 100))
			case dream.FixWriteFile:
				rewrote = append(rewrote, link(f.Path))
			case dream.FixRemoveFile:
				fmt.Fprintf(&b, "- Removed %s, which no longer had entries\n", link(f.Path))
			case dream.FixReplaceLine:
				switch {
				case f.New == "":
					fmt.Fprintf(&b, "- Removed a dead line from %s: `%s`\n", link(f.Path), clip(f.Old, 100))
				case f.Line == 1 && strings.HasPrefix(f.New, "---"):
					fmt.Fprintf(&b, "- Filled in the frontmatter of %s\n", link(f.Path))
				default:
					fmt.Fprintf(&b, "- Rewrote %s line %d\n", link(f.Path), f.Line)
				}
			}
		}
		if len(rewrote) > 0 {
			fmt.Fprintf(&b, "- Split the memory index so it fits what agents load at start: %s (every entry kept)\n", strings.Join(rewrote, ", "))
		}
		for _, s := range rep.Settled {
			fmt.Fprintf(&b, "- %s\n", s)
		}
		for _, a := range rep.Actions {
			fmt.Fprintf(&b, "- %s\n", a)
		}
		b.WriteString("\n")
	}
	if len(rep.Held) > 0 {
		fmt.Fprintf(&b, "## Held back (%d)\n\n", len(rep.Held))
		b.WriteString("Replaying past situations showed these edits would stop a useful memory from firing, so they were not made. Accept one with `grimoire memory replay --diff`.\n\n")
		for _, h := range rep.Held {
			fmt.Fprintf(&b, "- %s (%d fix(es)): %s\n", link(h.Path), h.Fixes, h.Reason)
		}
		b.WriteString("\n")
	}

	// The watch list: one line per kind of finding, not one per finding.
	type group struct {
		check string
		sev   dream.Severity
		n     int
		new   int
	}
	var groups []*group
	byCheck := map[string]*group{}
	for _, f := range rep.Findings {
		g := byCheck[f.Check]
		if g == nil {
			g = &group{check: f.Check, sev: f.Severity}
			byCheck[f.Check] = g
			groups = append(groups, g)
		}
		g.n++
		if severityRank[f.Severity] < severityRank[g.sev] {
			g.sev = f.Severity
		}
		if rep.New[f.Key()] {
			g.new++
		}
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if severityRank[groups[i].sev] != severityRank[groups[j].sev] {
			return severityRank[groups[i].sev] < severityRank[groups[j].sev]
		}
		return groups[i].n > groups[j].n
	})
	if len(groups) > 0 {
		b.WriteString("## Watching, no action needed\n\n")
		for _, g := range groups {
			fresh := ""
			if g.new > 0 {
				fresh = fmt.Sprintf(", %d new", g.new)
			}
			fmt.Fprintf(&b, "- %s: %d%s\n", watchLabel(g.check), g.n, fresh)
		}
		b.WriteString("\n## All findings\n\n")
		for _, f := range rep.Findings {
			where := link(f.Path)
			if f.Path == "" {
				where = ""
			}
			if f.Line > 0 {
				where += " line " + strconv.Itoa(f.Line)
			}
			mark := ""
			if rep.New[f.Key()] {
				mark = " (new)"
			}
			fmt.Fprintf(&b, "- %s `%s`%s %s — %s", f.Severity, f.Check, mark, where, f.Message)
			if f.Excerpt != "" {
				fmt.Fprintf(&b, " (`%s`)", clip(strings.ReplaceAll(f.Excerpt, "`", "'"), 120))
			}
			b.WriteString("\n")
		}
	} else if done == 0 && len(rep.Asks) == 0 {
		b.WriteString("Nothing to report.\n")
	}
	return b.String()
}

// watchLabel says in plain words what a kind of finding means.
func watchLabel(check string) string {
	if l, ok := map[string]string{
		"memory_quality":        "memories that break the writing guide (several facts in one, no date)",
		"pii":                   "notes that hold personal details such as an email address",
		"frontmatter":           "notes whose frontmatter the dream could not complete from the file name",
		"injection":             "lines that read like instructions to a model (usually describing one)",
		"procedure_verify":      "procedures naming paths that are gone; agents are told to re-check them on use",
		"dangerous_command":     "memories recording a risky command, such as a download piped to a shell",
		"stale_volatile":        "values that tend to change and have not been confirmed for months",
		"index_line_long":       "index lines longer than they need to be",
		"open_challenge":        "challenges to a person's fact, waiting out their first week",
		"duplicate":             "facts recorded twice",
		"near_duplicate":        "facts recorded twice in different words",
		"expired":               "facts past their own expiry",
		"unhelpful":             "memories marked unhelpful more often than helpful",
		"index_dangling":        "index lines pointing at missing files",
		"index_missing":         "notes not listed in the index",
		"index_truncated":       "index entries past the load limit",
		"untrusted_instruction": "rules learned from text other people can write",
	}[check]; ok {
		return l
	}
	return strings.ReplaceAll(check, "_", " ")
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

// indexOverLimit reports whether any MEMORY.md in the vault is past what
// agents load.
func (s *Server) indexOverLimit() bool {
	paths, err := s.Vault.Walk()
	if err != nil {
		return false
	}
	for _, root := range filemem.Roots(paths) {
		rel := "MEMORY.md"
		if root != "" {
			rel = root + "/MEMORY.md"
		}
		abs, err := s.Vault.SafePath(rel)
		if err != nil {
			continue
		}
		if b, err := os.ReadFile(abs); err == nil && filemem.OverLimit(string(b)) {
			return true
		}
	}
	return false
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
		apply := s.setting("dream_apply") != "off"
		// The interval waits for memory to settle, but an index past the load
		// limit is hiding memories from every session right now, and an agent
		// appends a line most days; that repair does not wait a day.
		due := time.Since(s.loadDreamState().At) >= time.Duration(hours*float64(time.Hour))
		if !due && !(apply && s.indexOverLimit()) {
			return
		}
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
