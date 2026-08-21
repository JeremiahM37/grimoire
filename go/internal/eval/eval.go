// Package eval measures retrieval on YOUR vault.
//
// The benchmarks in benchmarks/ are pre-registered, adversarially scored and
// run against public corpora — LoCoMo, LongMemEval — because that is the only
// way to publish a number anybody else can check. None of it helps the person
// who changes GRIMOIRE_EMBED_MODEL on a Tuesday and wants to know whether
// their own notes got easier or harder to find. That person currently has no
// instrument at all: they change the setting, ask a couple of questions, and
// decide from a feeling.
//
// So this is the same discipline, pointed inward. A question set is generated
// once from the vault and FROZEN; every later run scores against that frozen
// set, so a difference between two runs is a difference in retrieval and not
// in the questions.
//
// # Why there is no judge
//
// A question is generated FROM a specific chunk, and that chunk's identity is
// the gold answer. Scoring is then "did retrieval return that chunk", which is
// a set membership test — no reader, no judge, no model call, no sampling
// variance. That matters more here than it does in the published benchmarks:
// those measured an 8–12% answer-level flip rate from reader and judge
// sampling alone on byte-identical input, which is larger than most config
// changes anybody would make. A measurement whose noise floor exceeds the
// effect cannot answer the question it was built for.
//
// The cost of that choice is honest and stated: recall@k is not answer
// quality. A retriever can return the right chunk and a reader still answer
// badly. What recall@k measures is the thing a retrieval config actually
// controls, and it measures it exactly.
package eval

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Question is one probe: a query, and the chunk it was written from.
type Question struct {
	Q string `json:"q"`
	// Path and Chunk identify the gold passage. The pair, not the path alone:
	// a long note holds a dozen chunks and "somewhere in this note" is a far
	// easier target than the one the question was actually written from.
	Path  string `json:"path"`
	Chunk int    `json:"chunk"`
	// Excerpt is the text the question came from, kept so a person reading a
	// failure can see what was being asked about without opening the vault.
	Excerpt string `json:"excerpt,omitempty"`
}

// Set is a frozen question set.
type Set struct {
	Created string `json:"created"`
	// Generator is how the questions were written: "llm" or "lexical". It is
	// recorded because the two measure different things and their numbers are
	// NOT comparable — see Generate.
	Generator string     `json:"generator"`
	Model     string     `json:"model,omitempty"`
	Questions []Question `json:"questions"`
}

// Result is one scored run.
type Result struct {
	At        string `json:"at"`
	K         int    `json:"k"`
	Questions int    `json:"questions"`

	// RecallAtK is the fraction of questions whose gold chunk appeared in the
	// top k. The headline.
	RecallAtK float64 `json:"recall_at_k"`
	// NoteRecallAtK is the same test relaxed to the NOTE. Reported beside the
	// strict number because the two failing differently says what went wrong:
	// both low means retrieval missed the document, note-recall high with
	// chunk-recall low means it found the right note and the wrong part of it,
	// which is a chunking problem rather than an embedding one.
	NoteRecallAtK float64 `json:"note_recall_at_k"`
	// MRR is the mean reciprocal rank of the gold chunk, which moves when
	// recall does not: a change that lifts every right answer from rank 6 to
	// rank 2 is a real improvement and recall@8 cannot see it.
	MRR float64 `json:"mrr"`

	// Config is the retrieval configuration this run measured. Without it a
	// stored baseline is a number with no idea what produced it.
	Config Config `json:"config"`

	// Failures are the questions whose gold chunk was not returned, so the
	// answer to "it got worse" is a list of what to look at rather than a
	// smaller number.
	Failures []Failure `json:"failures,omitempty"`
}

// Failure is one missed question.
type Failure struct {
	Q        string   `json:"q"`
	Want     string   `json:"want"`
	Got      []string `json:"got"`
	NoteHit  bool     `json:"note_hit"`
	BestRank int      `json:"best_rank"` // rank of the gold NOTE, -1 if absent
}

// Config is what a run's numbers depend on.
type Config struct {
	Embedder string `json:"embedder"`
	Dim      int    `json:"dim"`
	Chunks   int    `json:"chunks"`
	Notes    int    `json:"notes"`
}

// Same reports whether two configs would be expected to produce the same
// numbers. Corpus size is deliberately NOT part of it: a vault that grew by
// three notes is still the same configuration, and refusing to compare across
// it would make the tool useless on a vault anybody is actually using.
func (c Config) Same(o Config) bool {
	return c.Embedder == o.Embedder && c.Dim == o.Dim
}

// Retriever is the slice of the index eval needs.
type Retriever interface {
	Rank(query string, k int) ([]Passage, error)
}

// Passage is one returned chunk, reduced to its identity.
type Passage struct {
	Path  string
	Chunk int
}

// Score runs a frozen set against a retriever.
//
// k is the depth a hit counts at. It is recorded in the result because
// recall@8 and recall@20 are different measurements and a stored baseline that
// did not say which it was would silently compare them.
func Score(set Set, r Retriever, k int, cfg Config) (Result, error) {
	if k <= 0 {
		k = 8
	}
	res := Result{
		At: Now().UTC().Format(time.RFC3339), K: k,
		Questions: len(set.Questions), Config: cfg,
	}
	if len(set.Questions) == 0 {
		return res, nil
	}
	hits, noteHits := 0, 0
	rrSum := 0.0
	for _, q := range set.Questions {
		got, err := r.Rank(q.Q, k)
		if err != nil {
			return res, err
		}
		rank, noteRank := -1, -1
		for i, p := range got {
			if p.Path == q.Path && noteRank < 0 {
				noteRank = i
			}
			if p.Path == q.Path && p.Chunk == q.Chunk {
				rank = i
				break
			}
		}
		if rank >= 0 {
			hits++
			rrSum += 1 / float64(rank+1)
		}
		if noteRank >= 0 {
			noteHits++
		}
		if rank < 0 {
			f := Failure{Q: q.Q, Want: passageID(q.Path, q.Chunk),
				NoteHit: noteRank >= 0, BestRank: noteRank}
			for i, p := range got {
				if i == 3 {
					break
				}
				f.Got = append(f.Got, passageID(p.Path, p.Chunk))
			}
			res.Failures = append(res.Failures, f)
		}
	}
	n := float64(len(set.Questions))
	res.RecallAtK = round(float64(hits) / n)
	res.NoteRecallAtK = round(float64(noteHits) / n)
	res.MRR = round(rrSum / n)
	return res, nil
}

func passageID(path string, chunk int) string {
	return fmt.Sprintf("%s#%d", path, chunk)
}

func round(f float64) float64 {
	return float64(int(f*10000+0.5)) / 10000
}

// Now is the clock, injectable so a test can pin a result's timestamp.
var Now = time.Now

// ------------------------------------------------------------- comparison

// Comparison is a baseline against a fresh run.
type Comparison struct {
	Baseline Result `json:"baseline"`
	Current  Result `json:"current"`

	RecallDelta float64 `json:"recall_delta"`
	MRRDelta    float64 `json:"mrr_delta"`

	// Fixed and Broken are the questions whose outcome CHANGED. This is the
	// part a person can act on: two runs that both score 0.72 can disagree
	// about a third of their questions, and "no change" would be a lie about
	// what happened.
	Fixed  []string `json:"fixed,omitempty"`
	Broken []string `json:"broken,omitempty"`

	// SameConfig is false when the two runs used different retrieval
	// backends. The comparison is still shown — that is usually the reason
	// somebody ran it — but a caller must be able to say so rather than
	// present it as a like-for-like regression.
	SameConfig bool `json:"same_config"`
}

// Compare diffs two runs of the same question set.
func Compare(baseline, current Result) Comparison {
	c := Comparison{
		Baseline: baseline, Current: current,
		RecallDelta: round(current.RecallAtK - baseline.RecallAtK),
		MRRDelta:    round(current.MRR - baseline.MRR),
		SameConfig:  baseline.Config.Same(current.Config),
	}
	was := map[string]bool{}
	for _, f := range baseline.Failures {
		was[f.Q] = true
	}
	now := map[string]bool{}
	for _, f := range current.Failures {
		now[f.Q] = true
	}
	for q := range was {
		if !now[q] {
			c.Fixed = append(c.Fixed, q)
		}
	}
	for q := range now {
		if !was[q] {
			c.Broken = append(c.Broken, q)
		}
	}
	// Sorted: map iteration order is random, and a report that reshuffles its
	// own rows between identical runs looks like it found something.
	sort.Strings(c.Fixed)
	sort.Strings(c.Broken)
	return c
}

// ------------------------------------------------------------- persistence

// WriteSet stores a question set as JSON.
func WriteSet(w io.Writer, s Set) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(s)
}

// ReadSet loads a question set.
func ReadSet(r io.Reader) (Set, error) {
	var s Set
	err := json.NewDecoder(r).Decode(&s)
	if err != nil {
		return Set{}, err
	}
	if len(s.Questions) == 0 {
		return s, fmt.Errorf("question set is empty")
	}
	return s, nil
}

// WriteResult stores a scored run.
func WriteResult(w io.Writer, res Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

// ReadResult loads a scored run.
func ReadResult(r io.Reader) (Result, error) {
	var res Result
	err := json.NewDecoder(r).Decode(&res)
	return res, err
}

// ------------------------------------------------------------- sampling

// pick chooses n items deterministically from a sorted list of ids.
//
// Deterministic, and not by position: taking every m-th chunk would sample the
// vault's alphabet rather than its content, so a folder named "aaa-archive"
// would dominate every set. Hashing the id gives an arbitrary but STABLE
// order, so the same vault produces the same sample twice — which is what lets
// a set be regenerated and still be recognisably the same measurement.
func pick(ids []string, n int) []string {
	if n <= 0 || n >= len(ids) {
		out := append([]string(nil), ids...)
		sort.Strings(out)
		return out
	}
	type scored struct {
		id string
		h  uint64
	}
	all := make([]scored, len(ids))
	for i, id := range ids {
		sum := sha256.Sum256([]byte(id))
		all[i] = scored{id: id, h: binary.BigEndian.Uint64(sum[:8])}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].h != all[j].h {
			return all[i].h < all[j].h
		}
		return all[i].id < all[j].id
	})
	out := make([]string, 0, n)
	for _, s := range all[:n] {
		out = append(out, s.id)
	}
	sort.Strings(out)
	return out
}

// Summary renders a result for a terminal.
func (r Result) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "recall@%d   %.1f%%   (%d questions)\n",
		r.K, r.RecallAtK*100, r.Questions)
	fmt.Fprintf(&b, "note recall %.1f%%\n", r.NoteRecallAtK*100)
	fmt.Fprintf(&b, "MRR         %.3f\n", r.MRR)
	fmt.Fprintf(&b, "embedder    %s (%d dims), %d chunks in %d notes\n",
		r.Config.Embedder, r.Config.Dim, r.Config.Chunks, r.Config.Notes)
	return b.String()
}
