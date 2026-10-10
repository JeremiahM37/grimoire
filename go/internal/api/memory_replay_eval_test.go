package api

// Evaluation of memory replay against the memory-use benchmark (docs/MEMORY_REPLAY.md).
// It is skipped unless REPLAY_EVAL_VAULT names a copy of the benchmark vault.
//
//	REPLAY_EVAL_VAULT   a copy of the vault (it is modified and restored)
//	REPLAY_EVAL_CASES   directory holding cases.json (round 1) and cases2.json (round 2)
//	REPLAY_EVAL_OUT     where to write the per-trial results (JSON lines)
//	REPLAY_EVAL_OLLAMA  Ollama URL for nomic-embed-text
//	REPLAY_EVAL_N       trials per kind (default 50)
//
// The replay corpus is round 1. Ground truth is the real /api/memory/context
// endpoint on the changed vault: in-sample on round 1 and held out on round 2.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/replay"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// memoEmb caches embeddings by text, on disk, so the same query is embedded
// once across runs and trials.
type memoEmb struct {
	inner *embed.Ollama
	mu    sync.Mutex
	m     map[string][]float32
	path  string
	dirty int
}

func (e *memoEmb) Signature() string { return e.inner.Signature() }
func (e *memoEmb) Dim() int          { return 0 }
func (e *memoEmb) Embed(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	var need []string
	e.mu.Lock()
	for i, t := range texts {
		if v, ok := e.m[t]; ok {
			out[i] = v
		} else {
			need = append(need, t)
		}
	}
	e.mu.Unlock()
	if len(need) > 0 {
		var uniq []string
		seen := map[string]bool{}
		for _, t := range need {
			if !seen[t] {
				seen[t] = true
				uniq = append(uniq, t)
			}
		}
		for lo := 0; lo < len(uniq); lo += 32 {
			hi := lo + 32
			if hi > len(uniq) {
				hi = len(uniq)
			}
			vecs := e.inner.Embed(uniq[lo:hi])
			e.mu.Lock()
			for k, v := range vecs {
				if len(v) > 0 {
					e.m[uniq[lo+k]] = v
					e.dirty++
				}
			}
			e.mu.Unlock()
		}
		e.mu.Lock()
		for i, t := range texts {
			if out[i] == nil {
				out[i] = e.m[t]
			}
		}
		e.mu.Unlock()
	}
	return out
}

func (e *memoEmb) save() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.dirty == 0 {
		return
	}
	raw, _ := json.Marshal(e.m)
	os.WriteFile(e.path, raw, 0o644)
	e.dirty = 0
}

type evalCase struct {
	MemID string `json:"mem_id"`
	Kind  string `json:"kind"`
	// Prompt is the user request; for an action it is the tool input.
	Prompt string `json:"prompt"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Cwd    string `json:"cwd"`
	Round  int    `json:"-"`
}

func (c evalCase) query() string {
	if c.Kind == "pos_action" {
		return c.Tool + " " + c.Input
	}
	return c.Prompt
}

func (c evalCase) stage() string {
	if c.Kind == "pos_action" {
		return "action"
	}
	return "prompt"
}

// evalTarget maps a benchmark memory id to the target an injection logs.
func evalTarget(memID string) string {
	store, key, _ := strings.Cut(memID, ":")
	switch store {
	case "am":
		return "note:Agent Memory/" + strings.SplitN(key, "#", 2)[0] + ".md"
	case "cm":
		return "note:Instructions/cm_" + key + ".md"
	}
	return "fact:" + key
}

func loadEvalCases(t *testing.T, dir string) []evalCase {
	var all []evalCase
	for round, name := range []string{"cases.json", "cases2.json"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var cs []evalCase
		if err := json.Unmarshal(raw, &cs); err != nil {
			t.Fatal(err)
		}
		for i := range cs {
			cs[i].Round = round + 1
		}
		all = append(all, cs...)
	}
	return all
}

type evalEnv struct {
	t     *testing.T
	s     *Server
	h     http.Handler
	emb   *memoEmb
	root  string
	cases []evalCase
}

func openEvalEnv(t *testing.T, root, casesDir, ollama string) *evalEnv {
	v, err := vault.New(root)
	if err != nil {
		t.Fatal(err)
	}
	gdir := filepath.Join(root, ".grimoire")
	database, err := db.Open(filepath.Join(gdir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	emb := &memoEmb{inner: embed.NewOllama(ollama, "nomic-embed-text"), m: map[string][]float32{},
		path: filepath.Join(filepath.Dir(root), "embcache.json")}
	if raw, err := os.ReadFile(emb.path); err == nil {
		json.Unmarshal(raw, &emb.m)
	}
	t.Cleanup(emb.save)
	vs := secrets.New(gdir)
	st := settings.New(gdir)
	ix := index.New(database, v, emb)
	cr := crdtstore.New(gdir)
	s := &Server{Index: ix, Vault: v, Settings: st, History: history.New(gdir), Secrets: vs,
		Broker: secrets.NewBroker(vs, database), CRDT: cr, AI: ai.New(st, vs.Get),
		Auth: auth.New(database), Sync: gsync.New(ix, v, cr), DailyDir: "journal", InboxDir: "inbox"}
	ix.Spaces = s
	return &evalEnv{t: t, s: s, h: s.Routes(), emb: emb, root: root, cases: loadEvalCases(t, casesDir)}
}

// fire asks the real endpoint what it would inject, the way the hook does.
func (e *evalEnv) fire(c evalCase) []string {
	v := url.Values{"q": {c.query()}, "rank": {"hybrid"}, "cwd": {c.Cwd}}
	if c.Kind == "pos_action" {
		v.Set("stage", "action")
		v.Set("min_rel", "0.7")
		v.Set("limit", "2")
		v.Set("max_bytes", "1200")
	}
	req := httptest.NewRequest("GET", "/api/memory/context?"+v.Encode(), nil)
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	var out struct {
		Context string `json:"context"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	var targets []string
	for _, line := range strings.Split(out.Context, "\n") {
		if !strings.HasPrefix(line, "- m:") {
			continue
		}
		m := evalSourceRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[2] != "" {
			targets = append(targets, "fact:"+m[2])
		} else {
			targets = append(targets, "note:"+m[1])
		}
	}
	return targets
}

// evalSourceRE reads the "[path#id; ...]" a directive line ends with.
var evalSourceRE = regexp.MustCompile(`\[([^\]#;]+?)(?:#([0-9a-f]+))?(?:;[^\]]*)?\]$`)

func (e *evalEnv) fireAll() [][]string {
	out := make([][]string, len(e.cases))
	var wg sync.WaitGroup
	next := make(chan int, len(e.cases))
	for i := range e.cases {
		next <- i
	}
	close(next)
	for w := 0; w < 12; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				out[i] = e.fire(e.cases[i])
			}
		}()
	}
	wg.Wait()
	e.emb.save()
	return out
}

func has(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// truth compares two real runs. remap maps a replaced memory to its heir.
type truthCounts struct {
	LostR1, LostR2   int // positive cases that hit before and not after
	NewNegR1, NewNeg int // negative cases that began to fire the memory they must not
	LostCases        map[string]bool
}

func (e *evalEnv) truth(before, after [][]string, remap map[string]string) truthCounts {
	tc := truthCounts{LostCases: map[string]bool{}}
	for i, c := range e.cases {
		g := evalTarget(c.MemID)
		heir := g
		if r, ok := remap[g]; ok {
			heir = r
		}
		switch c.Kind {
		case "pos_prompt", "pos_action":
			if has(before[i], g) && !has(after[i], heir) {
				if c.Round == 1 {
					tc.LostR1++
				} else {
					tc.LostR2++
				}
				tc.LostCases[replay.ID(replay.Clean(c.query()), c.stage())] = true
			}
		case "neg_prompt":
			if !has(before[i], g) && has(after[i], g) {
				if c.Round == 1 {
					tc.NewNegR1++
				}
				tc.NewNeg++
			}
		}
	}
	return tc
}

// --- the change generators ------------------------------------------------

type evalNote struct {
	rel, raw, front, body string
}

func (e *evalEnv) readNote(rel string) evalNote {
	raw, err := os.ReadFile(filepath.Join(e.root, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	text := string(raw)
	front, body := "", text
	if strings.HasPrefix(text, "---\n") {
		if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
			front, body = text[:4+i+5], text[4+i+5:]
		}
	}
	return evalNote{rel: rel, raw: text, front: front, body: body}
}

func words(s string) []string { return strings.Fields(s) }

var fillers = strings.Fields("system process review update workflow later general status detail note thing handle config setup")

func editBody(r *rand.Rand, op string, body string, vocab []string) string {
	ws := words(body)
	switch op {
	case "truncate":
		n := len(ws) / 4
		if n < 6 {
			n = 6
		}
		if n > len(ws) {
			n = len(ws)
		}
		return strings.Join(ws[:n], " ") + "\n"
	case "drop30":
		var out []string
		for _, w := range ws {
			if r.Float64() > 0.30 {
				out = append(out, w)
			}
		}
		return strings.Join(out, " ") + "\n"
	case "shuffle":
		lines := strings.Split(strings.TrimSpace(body), "\n")
		r.Shuffle(len(lines), func(i, j int) { lines[i], lines[j] = lines[j], lines[i] })
		return strings.Join(lines, "\n") + "\n"
	case "swap25":
		out := make([]string, len(ws))
		for i, w := range ws {
			out[i] = w
			if len(w) >= 5 && r.Float64() < 0.25 {
				out[i] = vocab[r.Intn(len(vocab))]
			}
		}
		return strings.Join(out, " ") + "\n"
	case "dilute":
		extra := ""
		for i := 0; i < 3; i++ {
			extra += "\n\n" + strings.Join(pick(r, vocab, 40), " ")
		}
		return strings.TrimRight(body, "\n") + extra + "\n"
	default: // "cosmetic": whitespace and a trailing period; the harmless control
		return strings.ReplaceAll(strings.TrimRight(body, "\n"), "\n\n", "\n") + "\n"
	}
}

func pick(r *rand.Rand, from []string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = from[r.Intn(len(from))]
	}
	return out
}

type trialResult struct {
	Kind    string `json:"kind"`
	Op      string `json:"op"`
	Target  string `json:"target"`
	Other   string `json:"other,omitempty"`
	Near    bool   `json:"near,omitempty"`
	Hold    bool   `json:"hold"`
	Checked bool   `json:"checked"`
	// Replay's own numbers.
	LostSituations int     `json:"replay_lost_situations"`
	LostMemories   int     `json:"replay_lost_memories"`
	NewFalse       int     `json:"replay_new_false"`
	AtRisk         int     `json:"replay_at_risk"`
	Score          float64 `json:"replay_score"`
	ReplayMs       int64   `json:"replay_ms"`
	// Same merge, replayed without carrying A's identity.
	HoldNoRemap bool `json:"hold_no_remap,omitempty"`
	LostNoRemap int  `json:"lost_no_remap,omitempty"`
	// Ground truth from the real endpoint.
	TruthLostR1 int `json:"truth_lost_r1"`
	TruthLostR2 int `json:"truth_lost_r2"`
	TruthNewNeg int `json:"truth_new_neg"`
	// Merge only: held-out loss if A's learned cues are NOT carried to B.
	TruthLostR2NoCarry int `json:"truth_lost_r2_no_carry,omitempty"`
	// Agreement of replay's lost situations with the real lost cases (round 1).
	AgreeBoth, AgreeOnlyReplay, AgreeOnlyReal int `json:"-"`
}

func TestReplayEval(t *testing.T) {
	dir := os.Getenv("REPLAY_EVAL_VAULT")
	if dir == "" {
		t.Skip("REPLAY_EVAL_VAULT not set")
	}
	n := 50
	if v, err := strconv.Atoi(os.Getenv("REPLAY_EVAL_N")); err == nil && v > 0 {
		n = v
	}
	ollama := os.Getenv("REPLAY_EVAL_OLLAMA")
	if ollama == "" {
		ollama = "http://100.127.85.58:11434"
	}
	t.Setenv("GRIMOIRE_REPLAY_MIN_USEFUL", "3")
	e := openEvalEnv(t, dir, os.Getenv("REPLAY_EVAL_CASES"), ollama)
	r := rand.New(rand.NewSource(20261009))
	outPath := os.Getenv("REPLAY_EVAL_OUT")
	var outFile *os.File
	if outPath != "" {
		var err error
		if outFile, err = os.Create(outPath); err != nil {
			t.Fatal(err)
		}
		defer outFile.Close()
	}
	emit := func(v any) {
		if outFile != nil {
			b, _ := json.Marshal(v)
			outFile.Write(append(b, '\n'))
		}
	}

	// 1. Baseline with the store as it is, plus learned cues from round-1 misses.
	t0 := time.Now()
	base := e.fireAll()
	t.Logf("baseline pass over %d cases: %v", len(e.cases), time.Since(t0))
	e.s.cues()
	var learned []map[string]any
	for i, c := range e.cases {
		if c.Round == 1 && c.Kind == "pos_prompt" && !has(base[i], evalTarget(c.MemID)) {
			learned = append(learned, map[string]any{"target": evalTarget(c.MemID), "text": c.Prompt})
		}
	}
	byTarget := map[string][]string{}
	for _, l := range learned {
		byTarget[l["target"].(string)] = append(byTarget[l["target"].(string)], l["text"].(string))
	}
	for target, texts := range byTarget {
		rec := do(t, e.h, "POST", "/api/memory/cues", map[string]any{"target": target, "kind": "request", "source": "learned", "cues": limitStrings(texts, 16)})
		if rec.Code != 200 {
			t.Logf("cue for %s: %d", target, rec.Code)
		}
	}
	cueFiles := map[string][]byte{}
	for _, f := range []string{"cues.jsonl", "cues.vec"} {
		cueFiles[f], _ = os.ReadFile(filepath.Join(dir, ".grimoire", f))
	}
	base = e.fireAll()
	t.Logf("learned cues for %d memories (%d cues)", len(byTarget), len(learned))
	posHit := func(round int) (hit, total int) {
		for i, c := range e.cases {
			if c.Round == round && (c.Kind == "pos_prompt" || c.Kind == "pos_action") {
				total++
				if has(base[i], evalTarget(c.MemID)) {
					hit++
				}
			}
		}
		return
	}
	h1, t1 := posHit(1)
	h2, t2 := posHit(2)
	t.Logf("baseline gold hit: round 1 %d/%d, round 2 %d/%d", h1, t1, h2, t2)

	// 2. Seed the corpus from round 1 and check replay agrees with the endpoint.
	var seeds []map[string]any
	for _, c := range e.cases {
		if c.Round != 1 {
			continue
		}
		sd := map[string]any{"text": c.query(), "stage": c.stage()}
		if c.Kind == "pos_action" {
			sd["min_rel"] = 0.7
			sd["limit"] = 2
			sd["budget"] = 1200
		}
		if c.Kind == "neg_prompt" {
			sd["avoid"] = []string{evalTarget(c.MemID)}
		} else {
			sd["expect"] = []string{evalTarget(c.MemID)}
		}
		seeds = append(seeds, sd)
	}
	rec := do(t, e.h, "POST", "/api/memory/replay/seed", map[string]any{"cases": seeds})
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	t0 = time.Now()
	rp, err := e.s.replayer(true)
	if err != nil || rp == nil {
		t.Fatalf("replayer: %v", err)
	}
	t.Logf("replayer built over %d situations, %d memories: %v", rp.Situations(), rp.Base().Len(), time.Since(t0))
	e.emb.save()
	agree, differ, goldAgree, goldN := 0, 0, 0, 0
	parityExtra, parityMissing := 0, 0
	rpFire := rp.FireTargets()
	for i, c := range e.cases {
		if c.Round != 1 {
			continue
		}
		id := replay.ID(replay.Clean(c.query()), c.stage())
		got, ok := rpFire[id]
		if !ok {
			continue
		}
		if sameSet(got, base[i]) {
			agree++
		} else {
			differ++
			extra, missing := 0, 0
			for _, g := range got {
				if !has(base[i], g) {
					extra++
				}
			}
			for _, g := range base[i] {
				if !has(got, g) {
					missing++
				}
			}
			parityExtra += extra
			parityMissing += missing
			if differ <= 6 {
				t.Logf("parity diff [%s] %q\n   replay %v\n   real   %v", c.stage(), c.query(), got, base[i])
			}
		}
		if c.Kind != "neg_prompt" {
			goldN++
			if has(got, evalTarget(c.MemID)) == has(base[i], evalTarget(c.MemID)) {
				goldAgree++
			}
		}
	}
	t.Logf("replay vs endpoint on round 1: identical fire sets %d/%d; gold hit agrees %d/%d", agree, agree+differ, goldAgree, goldN)

	t.Logf("parity: replay fires %d memories the endpoint does not, and misses %d the endpoint fires", parityExtra, parityMissing)
	if os.Getenv("REPLAY_EVAL_SCORES") != "" {
		e.compareScores(t, rp)
	}
	if os.Getenv("REPLAY_EVAL_PARITY_ONLY") != "" {
		return
	}

	// 3. Candidate memories: notes that are gold in the benchmark.
	var gold []string
	seen := map[string]bool{}
	for _, c := range e.cases {
		g := evalTarget(c.MemID)
		if strings.HasPrefix(g, "note:") && !seen[g] {
			seen[g] = true
			gold = append(gold, strings.TrimPrefix(g, "note:"))
		}
	}
	sort.Strings(gold)
	var allNotes []string
	rows, _ := e.s.Index.DB.Query("SELECT path FROM notes WHERE path LIKE 'Agent Memory/%' OR path LIKE 'Instructions/%' ORDER BY path")
	for rows.Next() {
		var p string
		rows.Scan(&p)
		allNotes = append(allNotes, p)
	}
	rows.Close()
	vocab := fillers
	for _, p := range pick(r, allNotes, 40) {
		vocab = append(vocab, words(e.readNote(p).body)...)
	}
	t.Logf("%d gold notes, %d candidate notes", len(gold), len(allNotes))

	var results []trialResult
	apply := func(rel, text string) {
		os.WriteFile(filepath.Join(e.root, rel), []byte(text), 0o644)
		if _, err := e.s.Index.Upsert(rel); err != nil {
			t.Fatal(err)
		}
	}
	restore := func(saved ...evalNote) {
		for _, sv := range saved {
			apply(sv.rel, sv.raw)
		}
	}
	runReplay := func(req map[string]any) (ReplayGate, int64) {
		t0 := time.Now()
		rec := do(t, e.h, "POST", "/api/memory/replay", req)
		if rec.Code != 200 {
			t.Fatalf("replay: %s", rec.Body)
		}
		var g ReplayGate
		decode(t, rec, &g)
		return g, time.Since(t0).Milliseconds()
	}
	finish := func(tr *trialResult, g ReplayGate, ms int64, tc truthCounts) {
		tr.Hold, tr.Checked = g.Verdict.Hold, g.Verdict.Checked
		tr.LostSituations, tr.LostMemories, tr.NewFalse, tr.Score, tr.ReplayMs = g.Report.LostSituations, g.Report.LostMemories, g.Report.NewFalseFires, g.Report.Score, ms
		tr.AtRisk = g.Report.AtRisk
		tr.TruthLostR1, tr.TruthLostR2, tr.TruthNewNeg = tc.LostR1, tc.LostR2, tc.NewNeg
		for _, d := range g.Report.Diffs {
			if len(d.Lost) > 0 {
				if tc.LostCases[d.ID] {
					tr.AgreeBoth++
				} else {
					tr.AgreeOnlyReplay++
				}
			}
		}
		tr.AgreeOnlyReal = tc.LostR1 - tr.AgreeBoth
		results = append(results, *tr)
		emit(tr)
	}

	only := map[int]bool{}
	for _, f := range strings.Split(os.Getenv("REPLAY_EVAL_ONLY"), ",") {
		if i, err := strconv.Atoi(f); err == nil {
			only[i] = true
		}
	}
	skip := func(i int) bool { return len(only) > 0 && !only[i] }

	// 4. Wording edits.
	ops := []string{"truncate", "drop30", "shuffle", "swap25", "dilute", "cosmetic"}
	for i := 0; i < n; i++ {
		rel := gold[r.Intn(len(gold))]
		op := ops[i%len(ops)]
		note := e.readNote(rel)
		nb := editBody(r, op, note.body, vocab)
		if skip(i) {
			continue
		}
		g, ms := runReplay(map[string]any{"notes": []map[string]string{{"path": rel, "text": note.front + nb}}})
		apply(rel, note.front+nb)
		after := e.fireAll()
		tr := trialResult{Kind: "edit", Op: op, Target: rel}
		tcE := e.truth(base, after, nil)
		if os.Getenv("REPLAY_EVAL_DEBUG") != "" {
			e.debugMisses(t, i, rel, op, g, tcE, base, after, rpFire)
		}
		finish(&tr, g, ms, tcE)
		restore(note)
	}

	// 5. Dream-style merges: A is folded into B. Half the pairs are the
	// nearest neighbour of B (what a duplicate sweep would pick), half random.
	snap := rp.Base()
	for i := 0; i < n; i++ {
		b := gold[r.Intn(len(gold))]
		near := i%2 == 0
		a := ""
		if near {
			a = nearestNote(snap, "note:"+b, allNotes)
		}
		if a == "" {
			near = false
			for a == "" || a == b {
				a = allNotes[r.Intn(len(allNotes))]
			}
		}
		na, nb := e.readNote(a), e.readNote(b)
		merged := nb.front + strings.TrimRight(nb.body, "\n") + "\n\n" + strings.TrimSpace(na.body) + "\n"
		req := map[string]any{
			"notes": []map[string]string{{"path": b, "text": merged}, {"path": a, "text": ""}},
			"remap": map[string]string{"note:" + a: "note:" + b},
		}
		g, ms := runReplay(req)
		delete(req, "remap")
		gNo, _ := runReplay(req)
		remap := map[string]string{"note:" + a: "note:" + b}

		apply(b, merged)
		os.Remove(filepath.Join(e.root, a))
		e.s.Index.Remove(a)
		// Without carrying A's learned cues to B:
		afterNoCarry := e.fireAll()
		tcNo := e.truth(base, afterNoCarry, remap)
		// With them carried (what a real merge now does):
		e.s.carryCues("note:"+a, "note:"+b)
		after := e.fireAll()
		tc := e.truth(base, after, remap)
		tr := trialResult{Kind: "merge", Op: "fold", Target: b, Other: a, Near: near,
			HoldNoRemap: gNo.Verdict.Hold, LostNoRemap: gNo.Report.LostMemories, TruthLostR2NoCarry: tcNo.LostR2}
		finish(&tr, g, ms, tc)
		// Undo: files, index and the cue store.
		restore(nb)
		apply(a, na.raw)
		for f, b := range cueFiles {
			os.WriteFile(filepath.Join(dir, ".grimoire", f), b, 0o644)
		}
		e.s.cueOnce, e.s.cueStore = sync.Once{}, nil
	}

	report(t, results)
}

// debugMisses explains lost round-1 cases that replay did not report.
func (e *evalEnv) debugMisses(t *testing.T, trial int, rel, op string, g ReplayGate, tc truthCounts, before, after [][]string, rpFire map[string][]string) {
	reported := map[string]bool{}
	for _, d := range g.Report.Diffs {
		if len(d.Lost) > 0 {
			reported[d.ID] = true
		}
	}
	for i, c := range e.cases {
		id := replay.ID(replay.Clean(c.query()), c.stage())
		if c.Round != 1 || !tc.LostCases[id] || reported[id] {
			continue
		}
		gold := evalTarget(c.MemID)
		t.Logf("MISS trial %d (%s %s) [%s] %q\n   gold %s\n   replay-before %v (gold fires: %v)\n   real-before %v\n   real-after %v",
			trial, op, rel, c.stage(), c.query(), gold, rpFire[id], has(rpFire[id], gold), before[i], after[i])
	}
}

// compareScores sets the endpoint's own candidate scores beside replay's.
func (e *evalEnv) compareScores(t *testing.T, rp *replay.Replayer) {
	snap := rp.Base()
	var n, big int
	var sumAbs float64
	shown := 0
	for _, c := range e.cases {
		if c.Round != 1 || c.Kind == "pos_action" {
			continue
		}
		q := c.query()
		req := httptest.NewRequest("GET", "/api/memory/context?"+url.Values{"q": {q}, "rank": {"hybrid"}}.Encode(), nil)
		req = req.WithContext(context.WithValue(req.Context(), principalKey{}, &caller{principal: &auth.Principal{Unrestricted: true}}))
		items, err := e.s.hybridContext(req, q, contextTerms(q), nil)
		if err != nil {
			t.Fatal(err)
		}
		p := replay.Prepare(replay.Situation{Text: replay.Clean(q), Vec: e.emb.Embed([]string{replay.Clean(q)})[0]})
		scored := map[string]replay.Scored{}
		for _, sc := range snap.Score(p, nil) {
			scored[sc.Target] = sc
		}
		for _, it := range items {
			if it.score < 0.45 {
				continue
			}
			sc, ok := scored[itemTarget(it)]
			d := it.score - sc.Score
			if !ok {
				d = it.score
			}
			n++
			sumAbs += abs(d)
			if abs(d) > 0.05 {
				big++
				if shown < 12 {
					shown++
					t.Logf("score diff %.3f: endpoint %.3f replay %.3f (text %.3f cue %.3f) %s for %q", d, it.score, sc.Score, sc.Text, sc.Cue, itemTarget(it), q)
				}
			}
		}
	}
	t.Logf("candidate scores >= 0.45: %d compared, mean abs diff %.4f, %d differ by more than 0.05", n, sumAbs/float64(n), big)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

func limitStrings(xs []string, n int) []string {
	if len(xs) > n {
		return xs[:n]
	}
	return xs
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	return strings.Join(x, "\x00") == strings.Join(y, "\x00")
}

// nearestNote finds the note most similar to target among candidates.
func nearestNote(sn *replay.Snapshot, target string, candidates []string) string {
	it := sn.Item(target)
	if it == nil || len(it.Parts) == 0 {
		return ""
	}
	best, bestSim := "", -1.0
	for _, c := range candidates {
		ct := "note:" + c
		if ct == target {
			continue
		}
		o := sn.Item(ct)
		if o == nil || len(o.Parts) == 0 {
			continue
		}
		if sim := replay.Cosine(it.Parts[0].Vec, o.Parts[0].Vec); sim > bestSim {
			best, bestSim = c, sim
		}
	}
	return best
}

func report(t *testing.T, rs []trialResult) {
	type conf struct{ tp, fp, fn, tn int }
	add := func(c *conf, hold, harm bool) {
		switch {
		case hold && harm:
			c.tp++
		case hold && !harm:
			c.fp++
		case !hold && harm:
			c.fn++
		default:
			c.tn++
		}
	}
	line := func(name string, c conf) string {
		p, rc := 0.0, 0.0
		if c.tp+c.fp > 0 {
			p = float64(c.tp) / float64(c.tp+c.fp)
		}
		if c.tp+c.fn > 0 {
			rc = float64(c.tp) / float64(c.tp+c.fn)
		}
		return fmt.Sprintf("%-34s harmful %2d  held %2d  tp %2d fp %2d fn %2d tn %2d  precision %.2f recall %.2f",
			name, c.tp+c.fn, c.tp+c.fp, c.tp, c.fp, c.fn, c.tn, p, rc)
	}
	for _, kind := range []string{"edit", "merge", "all"} {
		var c1, c2, cAny, cUnc, cRisk, cRisk1 conf
		var harm1, harm2, n, ms int64
		var agreeB, agreeR, agreeO int
		for _, r := range rs {
			if kind != "all" && r.Kind != kind {
				continue
			}
			n++
			ms += r.ReplayMs
			add(&c1, r.Hold, r.TruthLostR1 > 0)
			add(&c2, r.Hold, r.TruthLostR2 > 0)
			add(&cAny, r.Hold, r.TruthLostR1+r.TruthLostR2 > 0)
			add(&cUnc, r.LostMemories > 0, r.TruthLostR1+r.TruthLostR2 > 0)
			add(&cRisk, r.LostMemories > 0 || r.AtRisk > 0, r.TruthLostR1+r.TruthLostR2 > 0)
			add(&cRisk1, r.LostMemories > 0 || r.AtRisk > 0, r.TruthLostR1 > 0)
			if r.TruthLostR1 > 0 {
				harm1++
			}
			if r.TruthLostR2 > 0 {
				harm2++
			}
			agreeB += r.AgreeBoth
			agreeR += r.AgreeOnlyReplay
			agreeO += r.AgreeOnlyReal
		}
		if n == 0 {
			continue
		}
		t.Logf("== %s: %d trials, mean replay %d ms", kind, n, ms/n)
		t.Log(line("gate vs round-1 truth (in-sample)", c1))
		t.Log(line("gate vs round-2 truth (held out)", c2))
		t.Log(line("gate vs either", cAny))
		t.Log(line("gate+risk vs round-1 truth", cRisk1))
		t.Log(line("gate+risk vs either", cRisk))
		t.Logf("   round-1 lost cases: real-and-replay %d, replay only %d, real only %d", agreeB, agreeR, agreeO)
	}
	var carry, nocarry, rmH, norH, rmFalse int
	for _, r := range rs {
		if r.Kind != "merge" {
			continue
		}
		carry += r.TruthLostR2
		nocarry += r.TruthLostR2NoCarry
		if r.Hold {
			rmH++
		}
		if r.HoldNoRemap {
			norH++
		}
		rmFalse += r.NewFalse
	}
	t.Logf("merges: held-out (round 2) lost recalls with cues carried %d, without %d; merges held with remap %d, without remap %d", carry, nocarry, rmH, norH)
}
