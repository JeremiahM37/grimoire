package rerank

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"
)

// DefaultModel is the cross-encoder the local reranker uses unless told
// otherwise: MiniLM-L6 fine-tuned on MS MARCO passage ranking — 22M
// parameters, 91 MB of fp32 weights, small enough to run on CPU per query.
const DefaultModel = "cross-encoder/ms-marco-MiniLM-L-6-v2"

// DefaultMaxLen is the default pair length in tokens, specials included. The
// model accepts 512, but attention is quadratic and note chunks are far
// shorter, so 256 keeps the worst case bounded without truncating typical
// candidates.
const DefaultMaxLen = 256

// defaultBatchTokens bounds the tokens in one forward pass. Activations cost
// about 10 × hidden floats per token (~15 KB here), so 4096 tokens is ~60 MB
// of scratch however many documents a call scores.
const defaultBatchTokens = 4096

// retryAfter spaces out attempts to fetch or load a model that failed, so an
// offline server does not retry a download on every query.
const retryAfter = 10 * time.Minute

// LocalConfig configures the pure-Go cross-encoder.
type LocalConfig struct {
	// Model is a HuggingFace repo id (fetched into CacheDir on first use) or
	// a path to a directory holding config.json, tokenizer.json and
	// model.safetensors. Empty means DefaultModel.
	Model string
	// CacheDir is where fetched models are stored, normally
	// <grimoire data dir>/models.
	CacheDir string
	// AllowDownload permits fetching a missing model. Off, a missing model is
	// an error from Score rather than a network request.
	AllowDownload bool
	// MaxLen caps each (query, document) pair in tokens, specials included;
	// longer pairs are truncated longest-first. 0 means DefaultMaxLen.
	MaxLen int
	// Threads is the parallelism of a forward pass; 0 means GOMAXPROCS.
	Threads int
	// BatchTokens caps the tokens in one forward pass; 0 means 4096.
	BatchTokens int
}

// Local is a cross-encoder reranker running BERT inference in pure Go. The
// model is fetched and loaded on the first Score, not at construction, so
// enabling reranking costs nothing until it is used. Safe for concurrent use.
type Local struct {
	cfg LocalConfig

	mu      sync.Mutex
	model   *bertModel
	tok     *pairTokenizer
	loadErr error
	tried   time.Time

	pool sync.Pool // *workspace
}

// NewLocal returns a reranker that loads its model on first use.
func NewLocal(cfg LocalConfig) *Local {
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.MaxLen <= 0 {
		cfg.MaxLen = DefaultMaxLen
	}
	if cfg.BatchTokens <= 0 {
		cfg.BatchTokens = defaultBatchTokens
	}
	return &Local{cfg: cfg}
}

// LoadLocal loads a model directory now, returning any error immediately —
// for callers (and tests) that want failure at startup instead of first use.
func LoadLocal(dir string, cfg LocalConfig) (*Local, error) {
	cfg.Model = dir
	l := NewLocal(cfg)
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

// Name identifies the backend and model.
func (l *Local) Name() string { return "local:" + l.cfg.Model }

func (l *Local) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.model != nil {
		return nil
	}
	if l.loadErr != nil && time.Since(l.tried) < retryAfter {
		return l.loadErr
	}
	l.tried = time.Now()
	l.loadErr = l.loadLocked()
	return l.loadErr
}

func (l *Local) loadLocked() error {
	dir, err := ensureModel(l.cfg.Model, l.cfg.CacheDir, l.cfg.AllowDownload)
	if err != nil {
		return err
	}
	tok, err := loadPairTokenizer(dir)
	if err != nil {
		return err
	}
	m, err := loadBert(dir)
	if err != nil {
		return err
	}
	if l.cfg.MaxLen > m.cfg.MaxPositions {
		l.cfg.MaxLen = m.cfg.MaxPositions
	}
	if l.cfg.MaxLen < pairSpecials+2 {
		return fmt.Errorf("rerank max length %d is too short for a pair", l.cfg.MaxLen)
	}
	l.model, l.tok = m, tok
	return nil
}

func (l *Local) threads() int {
	if l.cfg.Threads > 0 {
		return l.cfg.Threads
	}
	return runtime.GOMAXPROCS(0)
}

// Score returns one relevance logit per document.
func (l *Local) Score(ctx context.Context, query string, docs []string) ([]float32, error) {
	if len(docs) == 0 {
		return []float32{}, nil
	}
	if err := l.load(); err != nil {
		return nil, fmt.Errorf("local reranker unavailable: %w", err)
	}
	threads := l.threads()

	ids := make([][]int32, len(docs))
	types := make([][]int32, len(docs))
	parallelTasks(threads, len(docs), func(_, i int) {
		ids[i], types[i] = l.tok.encodePair(query, docs[i], l.cfg.MaxLen)
	})

	ws, _ := l.pool.Get().(*workspace)
	if ws == nil {
		ws = &workspace{}
	}
	defer l.pool.Put(ws)

	out := make([]float32, 0, len(docs))
	var b batch
	flush := func() {
		if len(b.seqs) == 0 {
			return
		}
		out = append(out, l.model.forward(&b, ws, threads)...)
		b.ids, b.types, b.seqs = b.ids[:0], b.types[:0], b.seqs[:0]
	}
	for i := range docs {
		if len(b.ids)+len(ids[i]) > l.cfg.BatchTokens {
			flush()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		b.seqs = append(b.seqs, span{start: len(b.ids), n: len(ids[i])})
		b.ids = append(b.ids, ids[i]...)
		b.types = append(b.types, types[i]...)
	}
	flush()
	return out, nil
}
