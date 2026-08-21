package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/eval"
	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// grimoire eval — measure retrieval on YOUR vault.
//
// Three subcommands, and the split is the point:
//
//	eval build    write a question set from the vault, once, and freeze it
//	eval run      score that frozen set against the current configuration
//	eval compare  diff a run against the stored baseline
//
// Building and scoring are separate commands rather than one because a set
// that is regenerated on every run cannot measure a change: two runs would
// differ in their questions as well as their retrieval, and there would be no
// way to tell which moved. Freezing the set is the entire methodology.
//
// See internal/eval for why scoring uses no judge and no reader.

// evalDir is where sets and baselines live: inside .grimoire, with the index,
// because they are derived from the vault and rebuildable from it — not beside
// the notes, where they would sync to somebody's phone.
func evalDir(root string) string { return filepath.Join(root, ".grimoire", "eval") }

func cmdEval(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, evalUsage)
		return 2
	}
	switch args[0] {
	case "build":
		return evalBuild(args[1:])
	case "run":
		return evalRun(args[1:])
	case "compare":
		return evalCompare(args[1:])
	default:
		return fail("unknown eval subcommand %q\n%s", args[0], evalUsage)
	}
}

const evalUsage = `grimoire eval — measure retrieval on your own vault

  grimoire eval build [--n 50] [--set NAME] [--lexical]
        write a question set from the vault and freeze it
  grimoire eval run [--set NAME] [--k 8] [--baseline]
        score the frozen set against the current configuration
  grimoire eval compare [--set NAME] [--k 8]
        score now and diff against the stored baseline

Sets and baselines live in <vault>/.grimoire/eval/.`

func evalBuild(args []string) int {
	rest, lexical := flagOut(args, "--lexical")
	name := setName(rest)
	n := 50
	if v, ok := flagValue(rest, "--n"); ok {
		k, err := strconv.Atoi(v)
		if err != nil || k <= 0 {
			return fail("--n takes a positive number")
		}
		n = k
	}

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	chunks, err := evalChunks(e)
	if err != nil {
		return fail("%v", err)
	}

	var writer eval.Writer
	if !lexical {
		client := e.server.AI
		if client != nil && client.Available() {
			writer = &aiWriter{client: client}
		} else {
			// Said out loud rather than silently falling back. The two
			// generators measure different things, and somebody who thinks
			// they ran the semantic eval and actually ran the lexical floor
			// will draw the wrong conclusion from every later comparison.
			fmt.Fprintln(os.Stderr,
				"no LLM configured — writing a LEXICAL question set (a retrieval "+
					"floor, not a semantic test). Configure GRIMOIRE_OLLAMA_URL or "+
					"GRIMOIRE_LLM for the stronger set, or pass --lexical to silence this.")
		}
	}
	if writer != nil {
		fmt.Fprintf(os.Stderr, "writing questions with %s — this takes a while…\n",
			writer.Name())
	}

	set, err := eval.Generate(chunks, n, writer)
	if err != nil {
		return fail("%v", err)
	}
	path := filepath.Join(evalDir(e.vault.Root), name+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fail("%v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fail("%v", err)
	}
	defer f.Close()
	if err := eval.WriteSet(f, set); err != nil {
		return fail("%v", err)
	}
	fmt.Printf("wrote %d questions (%s) to %s\n", len(set.Questions), set.Generator, path)
	if len(set.Questions) < n {
		fmt.Printf("(asked for %d; the rest of the sampled passages had no "+
			"question in them)\n", n)
	}
	return 0
}

func evalRun(args []string) int {
	rest, asBaseline := flagOut(args, "--baseline")
	name := setName(rest)
	k := evalK(rest)
	if k < 0 {
		return fail("--k takes a positive number")
	}

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	set, res, code := scoreSet(e, name, k)
	if code != 0 {
		return code
	}
	fmt.Print(res.Summary())
	fmt.Printf("set         %s (%d questions, %s)\n", name, len(set.Questions), set.Generator)

	if len(res.Failures) > 0 {
		fmt.Printf("\n%d missed. The first few:\n", len(res.Failures))
		for i, f := range res.Failures {
			if i == 5 {
				break
			}
			where := "the note was not returned at all"
			if f.NoteHit {
				where = fmt.Sprintf("right note, wrong passage (note at rank %d)", f.BestRank+1)
			}
			fmt.Printf("  · %s\n    want %s — %s\n", f.Q, f.Want, where)
		}
	}
	if asBaseline {
		if err := saveBaseline(e.vault.Root, name, res); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("\nsaved as the baseline for %q\n", name)
	}
	return 0
}

func evalCompare(args []string) int {
	name := setName(args)
	k := evalK(args)
	if k < 0 {
		return fail("--k takes a positive number")
	}

	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()

	basePath := baselinePath(e.vault.Root, name)
	bf, err := os.Open(basePath)
	if err != nil {
		return fail("no baseline for %q — run `grimoire eval run --baseline` first", name)
	}
	baseline, err := eval.ReadResult(bf)
	bf.Close()
	if err != nil {
		return fail("reading %s: %v", basePath, err)
	}

	_, current, code := scoreSet(e, name, k)
	if code != 0 {
		return code
	}
	if baseline.K != current.K {
		// recall@8 and recall@20 are different measurements. Comparing them
		// would produce a confident number about nothing.
		return fail("the baseline was scored at k=%d and this run at k=%d — "+
			"rerun with --k %d, or take a new baseline", baseline.K, current.K, baseline.K)
	}

	c := eval.Compare(baseline, current)
	fmt.Printf("recall@%d   %.1f%%  →  %.1f%%   (%+.1f pp)\n", current.K,
		baseline.RecallAtK*100, current.RecallAtK*100, c.RecallDelta*100)
	fmt.Printf("MRR        %.3f  →  %.3f   (%+.3f)\n",
		baseline.MRR, current.MRR, c.MRRDelta)
	if !c.SameConfig {
		fmt.Printf("\nconfiguration CHANGED: %s (%d dims) → %s (%d dims)\n",
			baseline.Config.Embedder, baseline.Config.Dim,
			current.Config.Embedder, current.Config.Dim)
	}
	// The per-question diff, because two runs that both score 0.72 can
	// disagree about a third of their questions.
	fmt.Printf("\n%d fixed, %d broken\n", len(c.Fixed), len(c.Broken))
	for i, q := range c.Broken {
		if i == 5 {
			fmt.Printf("  … and %d more\n", len(c.Broken)-5)
			break
		}
		fmt.Printf("  broken: %s\n", q)
	}
	for i, q := range c.Fixed {
		if i == 5 {
			fmt.Printf("  … and %d more\n", len(c.Fixed)-5)
			break
		}
		fmt.Printf("  fixed:  %s\n", q)
	}
	return 0
}

// scoreSet loads a frozen set and scores it against the live index.
func scoreSet(e *env, name string, k int) (eval.Set, eval.Result, int) {
	path := filepath.Join(evalDir(e.vault.Root), name+".json")
	f, err := os.Open(path)
	if err != nil {
		return eval.Set{}, eval.Result{}, fail(
			"no question set %q — run `grimoire eval build` first", name)
	}
	set, err := eval.ReadSet(f)
	f.Close()
	if err != nil {
		return eval.Set{}, eval.Result{}, fail("reading %s: %v", path, err)
	}

	chunks, notes, _, err := e.index.CorpusStats(true)
	if err != nil {
		return set, eval.Result{}, fail("%v", err)
	}
	cfg := eval.Config{
		Embedder: e.embedder.Signature(), Dim: e.embedder.Dim(),
		Chunks: chunks, Notes: notes,
	}
	res, err := eval.Score(set, &indexRetriever{ix: e.index}, k, cfg)
	if err != nil {
		return set, eval.Result{}, fail("%v", err)
	}
	return set, res, 0
}

func saveBaseline(root, name string, res eval.Result) error {
	path := baselinePath(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return eval.WriteResult(f, res)
}

func baselinePath(root, name string) string {
	return filepath.Join(evalDir(root), name+".baseline.json")
}

func setName(args []string) string {
	if v, ok := flagValue(args, "--set"); ok {
		v = strings.TrimSpace(v)
		// A name is a filename. Anything that could climb out of the eval
		// directory is refused rather than sanitized, because a silently
		// renamed set is a comparison against the wrong baseline.
		if v != "" && !strings.ContainsAny(v, `/\.`) {
			return v
		}
		fmt.Fprintln(os.Stderr,
			"--set takes a simple name (letters, digits, dashes); using 'default'")
	}
	return "default"
}

// evalK returns the requested depth, or -1 when the flag is malformed.
func evalK(args []string) int {
	v, ok := flagValue(args, "--k")
	if !ok {
		return 8
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return -1
	}
	return n
}

// evalChunks reads the indexed passages the generator samples from.
//
// Private notes are INCLUDED: this is a local command run by the vault's owner
// against their own corpus, and excluding half of it would measure a corpus
// nobody retrieves from. Encrypted bodies never reach the vectors table at
// all, so there is nothing to exclude there.
func evalChunks(e *env) ([]eval.Chunk, error) {
	rows, err := e.index.DB.Query(
		"SELECT v.note, v.chunk_idx, v.chunk, COALESCE(n.title,'') " +
			"FROM vectors v LEFT JOIN notes n ON n.path = v.note " +
			"ORDER BY v.note, v.chunk_idx")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []eval.Chunk
	for rows.Next() {
		var c eval.Chunk
		if err := rows.Scan(&c.Path, &c.Index, &c.Text, &c.Title); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// indexRetriever adapts the index to the eval package, which knows nothing
// about spaces, trust or private notes — an eval run measures the corpus, not
// an access decision.
type indexRetriever struct{ ix *index.Index }

func (r *indexRetriever) Rank(query string, k int) ([]eval.Passage, error) {
	hits, err := r.ix.Retrieve(query, k, true)
	if err != nil {
		return nil, err
	}
	out := make([]eval.Passage, 0, len(hits))
	for _, h := range hits {
		out = append(out, eval.Passage{Path: h.Path, Chunk: h.ChunkIdx})
	}
	return out, nil
}

// aiWriter asks the configured model for a question.
type aiWriter struct{ client *ai.Client }

func (w *aiWriter) Name() string { return w.client.Backend() }

func (w *aiWriter) WriteQuestion(title, chunk string) (string, error) {
	prompt := eval.QuestionPrompt
	if strings.TrimSpace(title) != "" {
		prompt += "NOTE TITLE: " + title + "\n"
	}
	prompt += "PASSAGE:\n" + chunk + "\n\nQUESTION:"
	return w.client.Complete(prompt, w.client.Backend())
}
