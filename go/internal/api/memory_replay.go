package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/cues"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/replay"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Memory replay: regression tests for the memory store. Every hybrid context
// request is kept as a situation with the memories that fired and how each
// turned out; a proposed change is replayed against those situations before
// it lands. See docs/MEMORY_REPLAY.md.

type replayState struct {
	once  sync.Once
	store *replay.Store

	mu       sync.Mutex
	cached   *replay.Replayer
	cachedAt time.Time
	resolved time.Time
}

// replayTTL is how long a replayer built for a preview (a warning on a manual
// edit) is reused. Gates build a fresh one.
const replayTTL = 20 * time.Second

func (s *Server) replayDB() *replay.Store {
	s.replay.once.Do(func() {
		if s.Vault == nil || s.Index == nil || s.setting("replay_log") == "0" {
			return
		}
		lim := replay.Limits{
			MaxLive:   int(s.settingFloat("replay_max_situations", replay.DefaultMaxLive, 0, 1e6)),
			Retention: time.Duration(s.settingFloat("replay_retention_days", 60, 0, 3650)*24) * time.Hour,
		}
		st, err := replay.Open(filepath.Join(s.Vault.Root, ".grimoire"), lim)
		if err != nil {
			log.Printf("replay: %v", err)
			return
		}
		s.replay.store = st
	})
	return s.replay.store
}

// replayNote records a hybrid context request as a situation. It never delays
// or fails the response.
func (s *Server) replayNote(lg ctxLog, picked []contextItem) {
	st := s.replayDB()
	if st == nil || lg.session == "" || strings.TrimSpace(lg.query) == "" {
		return
	}
	fired := make([]replay.Fire, len(picked))
	for i, it := range picked {
		fired[i] = replay.Fire{Target: itemTarget(it), Relevance: it.score}
	}
	if err := st.Note(lg.session, lg.query, lg.stage, lg.minRel, lg.limit, fired, time.Now()); err != nil {
		log.Printf("replay: note: %v", err)
	}
}

// replayResolve folds the adherence log's outcomes into the corpus, at most
// once a minute.
func (s *Server) replayResolve() {
	st, adh := s.replayDB(), s.adh()
	if st == nil || adh == nil {
		return
	}
	s.replay.mu.Lock()
	if time.Since(s.replay.resolved) < time.Minute {
		s.replay.mu.Unlock()
		return
	}
	s.replay.resolved = time.Now()
	s.replay.mu.Unlock()
	rows, err := adh.RowsSince(time.Now().Add(-15 * 24 * time.Hour))
	if err != nil {
		return
	}
	var outs []replay.Outcome
	for _, r := range rows {
		o := r.Outcome()
		if o == "pending" {
			continue
		}
		outs = append(outs, replay.Outcome{Session: r.Session, Target: r.Target, Stage: r.Stage, Outcome: o, TS: r.TS})
	}
	if _, err := st.Resolve(outs, time.Now()); err != nil {
		log.Printf("replay: resolve: %v", err)
	}
}

// replaySnapshot reads the store as context injection sees it.
func (s *Server) replaySnapshot() (*replay.Snapshot, error) {
	facts, err := s.Index.ReplayFacts(vault.Now())
	if err != nil {
		return nil, err
	}
	chunks, err := s.Index.ReplayChunks()
	if err != nil {
		return nil, err
	}
	var items []*replay.Item
	for _, f := range facts {
		items = append(items, replay.NewItem(cues.FactTarget(f.ID), f.Text, []string{f.Text}, [][]float32{f.Vec}))
	}
	for i := 0; i < len(chunks); {
		j := i
		var texts []string
		var vecs [][]float32
		for j < len(chunks) && chunks[j].Note == chunks[i].Note {
			t := chunks[j].Text
			texts = append(texts, t)
			vecs = append(vecs, chunks[j].Vec)
			j++
		}
		items = append(items, replay.NewItem(cues.NoteTarget(chunks[i].Note), texts[0], texts, vecs))
		i = j
	}
	var recs []replay.CueRec
	if st := s.cues(); st != nil {
		all, vecs := st.Vectors()
		for i, c := range all {
			if i < len(vecs) {
				recs = append(recs, replay.NewCue(c, vecs[i]))
			}
		}
	}
	return replay.NewSnapshot(items, recs), nil
}

// replayer builds (or, unless fresh, reuses) a replayer over the corpus and
// the current store. A nil replayer with a nil error means there is no corpus.
func (s *Server) replayer(fresh bool) (*replay.Replayer, error) {
	st := s.replayDB()
	if st == nil || s.Index == nil || s.Index.Emb == nil {
		return nil, nil
	}
	s.replay.mu.Lock()
	if !fresh && s.replay.cached != nil && time.Since(s.replay.cachedAt) < replayTTL {
		rp := s.replay.cached
		s.replay.mu.Unlock()
		return rp, nil
	}
	s.replay.mu.Unlock()
	s.replayResolve()
	sits, err := st.Situations(s.Index.Emb, "")
	if err != nil {
		return nil, err
	}
	if len(sits) == 0 {
		return nil, nil
	}
	snap, err := s.replaySnapshot()
	if err != nil {
		return nil, err
	}
	rp := replay.NewReplayer(snap, sits)
	s.replay.mu.Lock()
	s.replay.cached, s.replay.cachedAt = rp, time.Now()
	s.replay.mu.Unlock()
	return rp, nil
}

// --- building a Change -----------------------------------------------------

type replayBuilder struct {
	s       *Server
	snap    *replay.Snapshot
	change  replay.Change
	pending []pendingItem // texts waiting for an embedding
}

type pendingItem struct {
	target, text string
	texts        []string
	vecAt        int // index of the first text in the batch
}

func (b *replayBuilder) want(target, text string, parts []string) {
	b.pending = append(b.pending, pendingItem{target: target, text: text, texts: parts})
}

// embedAll embeds every pending text in one call and turns them into items.
func (b *replayBuilder) embedAll() {
	var all []string
	for i := range b.pending {
		b.pending[i].vecAt = len(all)
		all = append(all, b.pending[i].texts...)
	}
	var vecs [][]float32
	if len(all) > 0 && b.s.Index.Emb != nil {
		vecs = b.s.Index.Emb.Embed(all)
	}
	for _, p := range b.pending {
		var vs [][]float32
		for k := range p.texts {
			if p.vecAt+k < len(vecs) {
				vs = append(vs, vecs[p.vecAt+k])
			}
		}
		b.change.Upsert = append(b.change.Upsert, replay.NewItem(p.target, p.text, p.texts, vs))
	}
	b.pending = nil
}

func (b *replayBuilder) remap(old, nw string) {
	if b.change.Remap == nil {
		b.change.Remap = map[string]string{}
	}
	b.change.Remap[old] = nw
}

func (b *replayBuilder) remove(t string) { b.change.Remove = append(b.change.Remove, t) }

// noteChange adds the effect of a note's body becoming newBody. oldBody is its
// current body ("" for a note that does not exist yet).
func (b *replayBuilder) noteChange(rel, oldBody, newBody string, exists bool) {
	if index.IsMemoryPath(rel) {
		b.memoryNoteChange(rel, oldBody, newBody)
		return
	}
	target := cues.NoteTarget(rel)
	if exists && !b.snap.Has(target) {
		return // private, untrusted or unindexed: injection never shows it
	}
	if strings.TrimSpace(newBody) == "" {
		if b.snap.Has(target) {
			b.remove(target)
		}
		return
	}
	title := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	if exists {
		var t string
		if b.s.Index.DB.QueryRow("SELECT title FROM notes WHERE path=?", rel).Scan(&t) == nil && t != "" {
			title = t
		}
	}
	chunks := embed.ChunkText(title + "\n\n" + newBody)
	if len(chunks) == 0 {
		return
	}
	b.want(target, chunks[0], chunks)
}

// memoryNoteChange diffs the facts of a memory note. Facts keep their id when
// the line keeps its id=; a rewritten fact usually mints a new one, so
// unmatched old and new facts are paired by word overlap and the pair is
// treated as the same memory (Remap), which is what carries cues and
// expectations to the rewritten fact.
func (b *replayBuilder) memoryNoteChange(rel, oldBody, newBody string) {
	now := vault.Now()
	live := func(body string) []memory.Entry {
		var out []memory.Entry
		for _, e := range memory.Parse(body) {
			if e.Live(now) && e.Challenges == "" && !e.Untrusted() {
				out = append(out, e)
			}
		}
		return out
	}
	oldE, newE := live(oldBody), live(newBody)
	newByID := map[string]memory.Entry{}
	for _, e := range newE {
		newByID[e.ID] = e
	}
	oldByID := map[string]memory.Entry{}
	var lostOld, freshNew []memory.Entry
	for _, e := range oldE {
		oldByID[e.ID] = e
		if _, ok := newByID[e.ID]; !ok {
			lostOld = append(lostOld, e)
		}
	}
	for _, e := range newE {
		o, ok := oldByID[e.ID]
		if !ok {
			freshNew = append(freshNew, e)
			continue
		}
		if o.Text != e.Text {
			b.want(cues.FactTarget(e.ID), e.Text, []string{e.Text})
		}
	}
	for _, p := range pairByWords(lostOld, freshNew) {
		if p.to >= 0 {
			nw := freshNew[p.to]
			b.want(cues.FactTarget(nw.ID), nw.Text, []string{nw.Text})
			b.remap(cues.FactTarget(lostOld[p.from].ID), cues.FactTarget(nw.ID))
		} else {
			b.remove(cues.FactTarget(lostOld[p.from].ID))
		}
	}
	paired := map[int]bool{}
	for _, p := range pairByWords(lostOld, freshNew) {
		if p.to >= 0 {
			paired[p.to] = true
		}
	}
	for i, e := range freshNew {
		if !paired[i] {
			b.want(cues.FactTarget(e.ID), e.Text, []string{e.Text})
		}
	}
}

type pairing struct{ from, to int } // to == -1: no counterpart

// pairByWords pairs old entries with new ones greedily by Jaccard overlap of
// their words, best first, at 0.4 or more.
func pairByWords(oldE, newE []memory.Entry) []pairing {
	type cand struct {
		i, j int
		sim  float64
	}
	set := func(t string) map[string]bool {
		m := map[string]bool{}
		for _, w := range memory.Tokens(t) {
			m[w] = true
		}
		return m
	}
	os, ns := make([]map[string]bool, len(oldE)), make([]map[string]bool, len(newE))
	for i, e := range oldE {
		os[i] = set(e.Text)
	}
	for j, e := range newE {
		ns[j] = set(e.Text)
	}
	var cs []cand
	for i := range oldE {
		for j := range newE {
			inter := 0
			for w := range os[i] {
				if ns[j][w] {
					inter++
				}
			}
			union := len(os[i]) + len(ns[j]) - inter
			if union > 0 && inter > 0 {
				if sim := float64(inter) / float64(union); sim >= 0.4 {
					cs = append(cs, cand{i, j, sim})
				}
			}
		}
	}
	sort.Slice(cs, func(a, c int) bool {
		if cs[a].sim != cs[c].sim {
			return cs[a].sim > cs[c].sim
		}
		if cs[a].i != cs[c].i {
			return cs[a].i < cs[c].i
		}
		return cs[a].j < cs[c].j
	})
	usedO, usedN := map[int]bool{}, map[int]bool{}
	match := map[int]int{}
	for _, c := range cs {
		if usedO[c.i] || usedN[c.j] {
			continue
		}
		usedO[c.i], usedN[c.j] = true, true
		match[c.i] = c.j
	}
	out := make([]pairing, len(oldE))
	for i := range oldE {
		if j, ok := match[i]; ok {
			out[i] = pairing{i, j}
		} else {
			out[i] = pairing{i, -1}
		}
	}
	return out
}

// noteBody returns a note's current body and whether it exists.
func (s *Server) noteBody(rel string) (string, bool) {
	var body string
	if s.Index.DB.QueryRow("SELECT body FROM notes WHERE path=?", rel).Scan(&body) != nil {
		return "", false
	}
	return body, true
}

// --- verdict and gate ------------------------------------------------------

// ReplayVerdict is what replay concludes about a change.
type ReplayVerdict struct {
	// Checked is false when the corpus holds too few useful recalls to judge.
	Checked bool   `json:"checked"`
	Hold    bool   `json:"hold"`
	Reason  string `json:"reason,omitempty"`
}

func (s *Server) replayJudge(rep *replay.Report) ReplayVerdict {
	need := int(s.settingFloat("replay_min_useful", 3, 0, 1e6))
	v := ReplayVerdict{Checked: true}
	if rep.Checked < need {
		v.Checked = false
		v.Reason = fmt.Sprintf("only %d situation(s) with a useful recall on file (need %d); change not checked", rep.Checked, need)
		return v
	}
	maxLost := 0
	if f, err := strconv.Atoi(s.setting("replay_max_lost")); err == nil && f >= 0 {
		maxLost = f
	}
	if rep.LostMemories > maxLost {
		v.Hold = true
		v.Reason = fmt.Sprintf("would lose %d useful recall(s) in %d situation(s)", rep.LostMemories, rep.LostSituations)
	}
	if f, err := strconv.Atoi(s.setting("replay_max_false")); err == nil && f >= 0 && rep.NewFalseFires > f {
		v.Hold = true
		if v.Reason != "" {
			v.Reason += "; "
		}
		v.Reason += fmt.Sprintf("would add %d false fire(s)", rep.NewFalseFires)
	}
	return v
}

// ReplayGate is the outcome of replaying one change.
type ReplayGate struct {
	Verdict ReplayVerdict  `json:"verdict"`
	Report  *replay.Report `json:"report,omitempty"`
}

// replayCheck diffs a change and judges it; the second return is the
// post-change snapshot, for Commit.
func (s *Server) replayCheck(rp *replay.Replayer, ch replay.Change) (ReplayGate, *replay.Snapshot) {
	rep, after := rp.Diff(ch)
	return ReplayGate{Verdict: s.replayJudge(rep), Report: rep}, after
}

// noteGater gates a series of automated note edits, one at a time, each judged
// against the store as the earlier ones left it.
type noteGater struct {
	s    *Server
	rp   *replay.Replayer
	init bool
}

func (s *Server) newNoteGater() *noteGater { return &noteGater{s: s} }

// check replays one note becoming newBody. It reports (gate, true) when
// replay had something to say; held says whether the edit must not be applied
// automatically. Failures to replay never block.
func (g *noteGater) check(rel, newBody string) (gate ReplayGate, held bool, apply func()) {
	apply = func() {}
	if g.s.setting("replay_gate") == "0" {
		return
	}
	if !g.init {
		g.init = true
		rp, err := g.s.replayer(true)
		if err != nil {
			log.Printf("replay: %v", err)
		}
		g.rp = rp
	}
	if g.rp == nil {
		return
	}
	b := &replayBuilder{s: g.s, snap: g.rp.Base()}
	oldBody, exists := g.s.noteBody(rel)
	b.noteChange(rel, oldBody, newBody, exists)
	b.embedAll()
	if b.change.Empty() {
		return
	}
	var after *replay.Snapshot
	gate, after = g.s.replayCheck(g.rp, b.change)
	held = gate.Verdict.Hold
	ch := b.change
	apply = func() {
		g.rp.Commit(ch, after)
		g.s.replayCarry(ch.Remap)
	}
	return
}

// replayCarry makes a real merge durable: the replaced memory's cues move to
// its replacement and the corpus follows the rename.
func (s *Server) replayCarry(remap map[string]string) {
	for old, nw := range remap {
		s.carryCues(old, nw)
		if st := s.replayDB(); st != nil {
			if err := st.Remap(old, nw); err != nil {
				log.Printf("replay: remap: %v", err)
			}
		}
	}
}

// carryCues copies a replaced memory's cues to its replacement.
func (s *Server) carryCues(old, nw string) {
	st := s.cues()
	if st == nil || old == "" || nw == "" || old == nw {
		return
	}
	cs := st.For(old)
	if len(cs) == 0 {
		return
	}
	moved := make([]cues.Cue, len(cs))
	for i, c := range cs {
		c.Target = nw
		moved[i] = c
	}
	if _, err := st.Add(moved); err != nil {
		log.Printf("replay: carrying cues: %v", err)
	}
}

// replayWarning is the short form attached to a manual edit's response.
type replayWarning struct {
	LostRecalls int      `json:"lost_recalls"`
	Situations  int      `json:"situations"`
	NewFalse    int      `json:"new_false_fires,omitempty"`
	Examples    []string `json:"examples,omitempty"`
	Note        string   `json:"note"`
}

// replayPreview replays a manual edit and returns a warning if it would lose
// useful recalls. It never blocks the edit and returns nil on any trouble.
func (s *Server) replayPreview(r *http.Request, build func(b *replayBuilder)) *replayWarning {
	if s.setting("replay_gate") == "0" {
		return nil
	}
	rp, err := s.replayer(false)
	if err != nil || rp == nil {
		return nil
	}
	b := &replayBuilder{s: s, snap: rp.Base()}
	build(b)
	b.embedAll()
	if b.change.Empty() {
		return nil
	}
	gate, _ := s.replayCheck(rp, b.change)
	if !gate.Verdict.Checked || gate.Report.LostMemories == 0 {
		return nil
	}
	w := &replayWarning{LostRecalls: gate.Report.LostMemories, Situations: gate.Report.LostSituations,
		NewFalse: gate.Report.NewFalseFires,
		Note:     "this edit stops a memory that was useful from firing in past situations; it was applied because manual edits are never blocked (grimoire memory replay --diff)"}
	if p := principal(r); p.Unrestricted || p.IsAdmin() {
		for _, d := range gate.Report.Diffs {
			if len(d.Lost) > 0 && len(w.Examples) < 3 {
				w.Examples = append(w.Examples, d.Text)
			}
		}
	}
	return w
}

// --- HTTP ------------------------------------------------------------------

type replayRequest struct {
	// Notes replace whole notes: path is vault-relative or an absolute path to
	// a file that is (a link to) a note; text is the new content.
	Notes []struct {
		Path string `json:"path"`
		Text string `json:"text"`
	} `json:"notes"`
	Edits []struct {
		Target string `json:"target"`
		Text   string `json:"text"`
	} `json:"edits"`
	Remove []string `json:"remove"`
	Merge  []struct {
		From []string `json:"from"`
		Into string   `json:"into"`
		Text string   `json:"text"`
	} `json:"merge"`
	Remap map[string]string `json:"remap"`
}

// resolveNotePath maps a path from a client to an indexed note path.
func (s *Server) resolveNotePath(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		return normPath(p)
	}
	root := strings.TrimRight(s.Vault.Root, "/") + "/"
	if strings.HasPrefix(p, root) {
		return strings.TrimPrefix(p, root)
	}
	want, err := os.Stat(p)
	if err != nil {
		return ""
	}
	rows, err := s.Index.DB.Query("SELECT path FROM notes WHERE path LIKE ?", "%"+filepath.Base(p))
	if err != nil {
		return ""
	}
	defer rows.Close()
	for rows.Next() {
		var rel string
		if rows.Scan(&rel) != nil {
			continue
		}
		if got, err := os.Stat(filepath.Join(s.Vault.Root, rel)); err == nil && os.SameFile(want, got) {
			return rel
		}
	}
	return ""
}

// replayRun replays a proposed change against the situation corpus without
// applying it. Administrators only: the answer quotes past requests.
//
//	POST /api/memory/replay  {notes, edits, remove, merge, remap}
func (s *Server) replayRun(w http.ResponseWriter, r *http.Request) {
	var in replayRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	rp, err := s.replayer(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if rp == nil {
		writeJSON(w, http.StatusOK, ReplayGate{Verdict: ReplayVerdict{Reason: "no situations on file yet; nothing to replay against"}})
		return
	}
	b := &replayBuilder{s: s, snap: rp.Base()}
	for _, n := range in.Notes {
		rel := s.resolveNotePath(n.Path)
		if rel == "" {
			writeErr(w, http.StatusBadRequest, "no indexed note for "+n.Path)
			return
		}
		old, exists := s.noteBody(rel)
		note := vault.NoteFromText(rel, n.Text, 0)
		b.noteChange(rel, old, note.Body, exists)
	}
	for _, e := range in.Edits {
		if _, ok := strings.CutPrefix(e.Target, "fact:"); !ok && !strings.HasPrefix(e.Target, "note:") {
			writeErr(w, http.StatusBadRequest, "edit target must be fact:<id> or note:<path>")
			return
		}
		if !b.snap.Has(e.Target) {
			writeErr(w, http.StatusBadRequest, "not an injectable memory: "+e.Target)
			return
		}
		b.want(e.Target, e.Text, []string{e.Text})
	}
	b.change.Remove = append(b.change.Remove, in.Remove...)
	for _, m := range in.Merge {
		if m.Into == "" || len(m.From) == 0 {
			writeErr(w, http.StatusBadRequest, "merge needs from and into")
			return
		}
		text := m.Text
		if text == "" {
			if it := b.snap.Item(m.Into); it != nil {
				text = it.Text
			}
		}
		if text != "" {
			b.want(m.Into, text, []string{text})
		}
		for _, f := range m.From {
			if f != m.Into {
				b.remap(f, m.Into)
			}
		}
	}
	for o, n := range in.Remap {
		b.remap(o, n)
	}
	b.embedAll()
	gate, _ := s.replayCheck(rp, b.change)
	if r.URL.Query().Get("detail") == "0" {
		gate.Report.Diffs = nil
	}
	writeJSON(w, http.StatusOK, gate)
}

// replayInfo describes the corpus.
//
//	GET /api/memory/replay
func (s *Server) replayInfo(w http.ResponseWriter, r *http.Request) {
	st := s.replayDB()
	if st == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	s.replayResolve()
	stats, err := st.Stats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": true, "stats": stats,
		"gate": s.setting("replay_gate") != "0"})
}

// replaySeed loads a seed suite: situations with the memory that must fire
// (expect) and ones that must not (avoid). Administrators only.
//
//	POST /api/memory/replay/seed  {"cases":[{"text","stage","min_rel","expect":[],"avoid":[]}]}
func (s *Server) replaySeed(w http.ResponseWriter, r *http.Request) {
	st := s.replayDB()
	if st == nil {
		writeErr(w, http.StatusServiceUnavailable, "replay is off")
		return
	}
	var in struct {
		Cases []struct {
			Text   string   `json:"text"`
			Stage  string   `json:"stage"`
			MinRel float64  `json:"min_rel"`
			Expect []string `json:"expect"`
			Avoid  []string `json:"avoid"`
		} `json:"cases"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	seeds := make([]replay.Seed, 0, len(in.Cases))
	for _, c := range in.Cases {
		seeds = append(seeds, replay.Seed{Text: c.Text, Stage: c.Stage, MinRel: c.MinRel, Expect: c.Expect, Avoid: c.Avoid})
	}
	added, err := st.AddSeeds(seeds, time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "received": len(seeds)})
}
