package rerank

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// bertConfig is the subset of a HuggingFace BertConfig the forward pass needs.
type bertConfig struct {
	HiddenSize       int               `json:"hidden_size"`
	NumLayers        int               `json:"num_hidden_layers"`
	NumHeads         int               `json:"num_attention_heads"`
	IntermediateSize int               `json:"intermediate_size"`
	MaxPositions     int               `json:"max_position_embeddings"`
	TypeVocabSize    int               `json:"type_vocab_size"`
	VocabSize        int               `json:"vocab_size"`
	LayerNormEps     float64           `json:"layer_norm_eps"`
	HiddenAct        string            `json:"hidden_act"`
	PositionEmbType  string            `json:"position_embedding_type"`
	ModelType        string            `json:"model_type"`
	ID2Label         map[string]string `json:"id2label"`
}

func loadConfig(dir string) (bertConfig, error) {
	var c bertConfig
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return c, fmt.Errorf("reading model config: %w", err)
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parsing model config: %w", err)
	}
	if c.LayerNormEps == 0 {
		c.LayerNormEps = 1e-12
	}
	switch {
	case c.ModelType != "" && c.ModelType != "bert":
		return c, fmt.Errorf("unsupported model_type %q (only bert)", c.ModelType)
	case c.HiddenAct != "" && c.HiddenAct != "gelu":
		return c, fmt.Errorf("unsupported hidden_act %q (only exact gelu)", c.HiddenAct)
	case c.PositionEmbType != "" && c.PositionEmbType != "absolute":
		return c, fmt.Errorf("unsupported position_embedding_type %q", c.PositionEmbType)
	case len(c.ID2Label) > 1:
		return c, fmt.Errorf("model has %d labels; a reranker needs a single relevance logit", len(c.ID2Label))
	case c.HiddenSize <= 0 || c.NumHeads <= 0 || c.HiddenSize%c.NumHeads != 0:
		return c, fmt.Errorf("hidden_size %d is not divisible into %d heads", c.HiddenSize, c.NumHeads)
	case c.NumLayers <= 0 || c.IntermediateSize <= 0 || c.MaxPositions <= 0 || c.VocabSize <= 0:
		return c, fmt.Errorf("model config is missing its dimensions")
	}
	if c.TypeVocabSize == 0 {
		c.TypeVocabSize = 2
	}
	return c, nil
}

// dense is a linear layer in PyTorch layout: w is [out × in], row-major, so a
// row of w is contiguous over the input dimension and y = x·wᵀ + b is a dot
// product of two contiguous vectors per output.
type dense struct {
	w       []float32
	b       []float32
	in, out int
}

type layerNorm struct{ g, b []float32 }

type bertLayer struct {
	qkv  dense // query, key and value fused into one [3H × H] projection
	attn dense // attention output projection
	ln1  layerNorm
	ffn1 dense
	ffn2 dense
	ln2  layerNorm
}

// bertModel is BertForSequenceClassification with a single label: encoder,
// tanh pooler over [CLS], and a linear classifier giving one logit.
type bertModel struct {
	cfg      bertConfig
	word     []float32 // [vocab × H]
	pos      []float32 // [maxPos × H]
	typ      []float32 // [types × H]
	embLN    layerNorm
	layers   []bertLayer
	pooler   dense
	classify dense
}

func loadBert(dir string) (*bertModel, error) {
	cfg, err := loadConfig(dir)
	if err != nil {
		return nil, err
	}
	st, err := openSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	defer st.Close()

	// Checkpoints saved from BertForSequenceClassification prefix the encoder
	// with "bert."; a bare BertModel export does not.
	prefix := "bert."
	if !st.has(prefix + "embeddings.word_embeddings.weight") {
		prefix = ""
	}
	H, I := cfg.HiddenSize, cfg.IntermediateSize
	var firstErr error
	get := func(name string, shape ...int) []float32 {
		if firstErr != nil {
			return nil
		}
		v, err := st.float32s(name, shape...)
		if err != nil {
			firstErr = err
		}
		return v
	}
	lin := func(name string, out, in int) dense {
		return dense{w: get(name+".weight", out, in), b: get(name+".bias", out), in: in, out: out}
	}
	ln := func(name string) layerNorm {
		return layerNorm{g: get(name+".weight", H), b: get(name+".bias", H)}
	}

	m := &bertModel{cfg: cfg}
	e := prefix + "embeddings."
	m.word = get(e+"word_embeddings.weight", cfg.VocabSize, H)
	m.pos = get(e+"position_embeddings.weight", cfg.MaxPositions, H)
	m.typ = get(e+"token_type_embeddings.weight", cfg.TypeVocabSize, H)
	m.embLN = ln(e + "LayerNorm")
	for l := 0; l < cfg.NumLayers; l++ {
		p := fmt.Sprintf("%sencoder.layer.%d.", prefix, l)
		q := lin(p+"attention.self.query", H, H)
		k := lin(p+"attention.self.key", H, H)
		v := lin(p+"attention.self.value", H, H)
		layer := bertLayer{
			attn: lin(p+"attention.output.dense", H, H),
			ln1:  ln(p + "attention.output.LayerNorm"),
			ffn1: lin(p+"intermediate.dense", I, H),
			ffn2: lin(p+"output.dense", H, I),
			ln2:  ln(p + "output.LayerNorm"),
		}
		if firstErr == nil {
			layer.qkv = dense{
				w:  append(append(q.w, k.w...), v.w...),
				b:  append(append(q.b, k.b...), v.b...),
				in: H, out: 3 * H,
			}
		}
		m.layers = append(m.layers, layer)
	}
	m.pooler = lin(prefix+"pooler.dense", H, H)
	m.classify = lin("classifier", 1, H)
	if firstErr != nil {
		return nil, firstErr
	}
	return m, nil
}

// ---------------------------------------------------------------- forward

// batch is a set of sequences packed back to back with no padding: row t of
// every activation matrix is one token, and seqs gives each sequence's rows.
// Linear layers treat the whole batch as one matrix; only attention needs the
// sequence boundaries. Packing does strictly less work than padding to the
// longest sequence, and no attention mask is needed.
type batch struct {
	ids, types []int32
	seqs       []span
}

type span struct{ start, n int }

// workspace holds the activation buffers for one forward pass, reused across
// calls through a pool so a steady stream of reranks does not churn the GC.
type workspace struct {
	hidden, qkv, ctx, tmp, ffn []float32
	attn                       []attnScratch // per worker
	cls                        []float32
}

func grow(s []float32, n int) []float32 {
	if cap(s) < n {
		return make([]float32, n)
	}
	return s[:n]
}

// forward returns one logit per sequence in b.
func (m *bertModel) forward(b *batch, ws *workspace, threads int) []float32 {
	cfg := m.cfg
	H, I := cfg.HiddenSize, cfg.IntermediateSize
	T := len(b.ids)
	nseq := len(b.seqs)

	ws.hidden = grow(ws.hidden, T*H)
	ws.qkv = grow(ws.qkv, T*3*H)
	ws.ctx = grow(ws.ctx, T*H)
	ws.tmp = grow(ws.tmp, T*H)
	ws.ffn = grow(ws.ffn, T*I)
	if len(ws.attn) < threads {
		ws.attn = append(ws.attn, make([]attnScratch, threads-len(ws.attn))...)
	}
	eps := float32(cfg.LayerNormEps)

	// embeddings: word + position + token type, then LayerNorm
	parallelRows(threads, T, 64, func(lo, hi int) {
		for t := lo; t < hi; t++ {
			row := ws.hidden[t*H : (t+1)*H]
			copy(row, m.word[int(b.ids[t])*H:])
			add(row, m.typ[int(b.types[t])*H:])
		}
	})
	for _, s := range b.seqs {
		for p := 0; p < s.n; p++ {
			add(ws.hidden[(s.start+p)*H:(s.start+p+1)*H], m.pos[p*H:])
		}
	}
	parallelRows(threads, T, 64, func(lo, hi int) {
		for t := lo; t < hi; t++ {
			layerNormRow(ws.hidden[t*H:(t+1)*H], nil, m.embLN, eps)
		}
	})

	last := len(m.layers) - 1
	for l, layer := range m.layers {
		linear(threads, ws.qkv, ws.hidden, T, &layer.qkv, actNone)
		if l < last {
			m.attention(threads, b.seqs, ws, false)
			linear(threads, ws.tmp, ws.ctx, T, &layer.attn, actNone)
			parallelRows(threads, T, 64, func(lo, hi int) {
				for t := lo; t < hi; t++ {
					layerNormRow(ws.hidden[t*H:(t+1)*H], ws.tmp[t*H:(t+1)*H], layer.ln1, eps)
				}
			})
			linear(threads, ws.ffn, ws.hidden, T, &layer.ffn1, actGELU)
			linear(threads, ws.tmp, ws.ffn, T, &layer.ffn2, actNone)
			parallelRows(threads, T, 64, func(lo, hi int) {
				for t := lo; t < hi; t++ {
					layerNormRow(ws.hidden[t*H:(t+1)*H], ws.tmp[t*H:(t+1)*H], layer.ln2, eps)
				}
			})
			continue
		}
		// The last layer's output is only read at [CLS] by the pooler, so
		// everything after attention runs on one row per sequence. Keys and
		// values still come from every token.
		m.attention(threads, b.seqs, ws, true) // ctx row i = sequence i's [CLS]
		ws.cls = grow(ws.cls, nseq*H)
		for i, s := range b.seqs {
			copy(ws.cls[i*H:(i+1)*H], ws.hidden[s.start*H:(s.start+1)*H])
		}
		hidden := ws.cls
		ctx := ws.ctx[:nseq*H]
		tmp := ws.tmp[:nseq*H]
		ffn := ws.ffn[:nseq*I]
		linear(threads, tmp, ctx, nseq, &layer.attn, actNone)
		for i := 0; i < nseq; i++ {
			layerNormRow(hidden[i*H:(i+1)*H], tmp[i*H:(i+1)*H], layer.ln1, eps)
		}
		linear(threads, ffn, hidden, nseq, &layer.ffn1, actGELU)
		linear(threads, tmp, ffn, nseq, &layer.ffn2, actNone)
		for i := 0; i < nseq; i++ {
			layerNormRow(hidden[i*H:(i+1)*H], tmp[i*H:(i+1)*H], layer.ln2, eps)
		}
	}

	// pooler (tanh over [CLS]) and the single-logit classifier
	pooled := ws.tmp[:nseq*H]
	linear(threads, pooled, ws.cls, nseq, &m.pooler, actTanh)
	out := make([]float32, nseq)
	linear(1, out, pooled, nseq, &m.classify, actNone)
	return out
}

// attnRows is how many query rows of one head are scored at a time, bounding
// the score block to attnRows × seqLen however long the sequence.
const attnRows = 32

// attnScratch is one worker's attention buffers.
type attnScratch struct {
	q, k, vt, s []float32
}

// attention computes multi-head scaled dot-product self-attention for every
// sequence from the fused projections in ws.qkv, writing the per-head
// contexts into ws.ctx. With clsOnly it computes only each sequence's first
// query row and writes it to ctx row i for sequence i.
//
// Each (sequence, head) task gathers its head's queries, keys and transposed
// values into contiguous blocks so both products — scores = q·kᵀ and
// context = softmax(scores)·v — run through the same vector kernel as the
// linear layers.
func (m *bertModel) attention(threads int, seqs []span, ws *workspace, clsOnly bool) {
	H := m.cfg.HiddenSize
	heads := m.cfg.NumHeads
	d := H / heads
	stride := 3 * H
	scale := float32(1 / math.Sqrt(float64(d)))
	qkv, ctx := ws.qkv, ws.ctx

	parallelTasks(threads, len(seqs)*heads, func(worker, task int) {
		si := task / heads
		s := seqs[si]
		h := task % heads
		n := s.n
		nq := n
		if clsOnly {
			nq = 1
		}
		sc := &ws.attn[worker]
		sc.q = grow(sc.q, nq*d)
		sc.k = grow(sc.k, n*d)
		sc.vt = grow(sc.vt, d*n)
		sc.s = grow(sc.s, min(nq, attnRows)*n)
		q, k, vt := sc.q, sc.k, sc.vt
		for j := 0; j < n; j++ {
			row := qkv[(s.start+j)*stride:]
			copy(k[j*d:(j+1)*d], row[H+h*d:H+h*d+d])
			v := row[2*H+h*d : 2*H+h*d+d]
			for c, x := range v {
				vt[c*n+j] = x
			}
			if j < nq {
				qj := q[j*d : (j+1)*d]
				for c, x := range row[h*d : h*d+d] {
					qj[c] = x * scale
				}
			}
		}
		for i0 := 0; i0 < nq; i0 += attnRows {
			rb := min(attnRows, nq-i0)
			scores := sc.s[:rb*n]
			gemmT(scores, n, q[i0*d:], d, k, d, rb, n, d)
			for i := 0; i < rb; i++ {
				softmax(scores[i*n : (i+1)*n])
			}
			orow := (s.start + i0) * H
			if clsOnly {
				orow = si * H
			}
			gemmT(ctx[orow+h*d:], H, scores, n, vt, n, rb, d, n)
		}
	})
}

func softmax(x []float32) {
	mx := float32(math.Inf(-1))
	for _, v := range x {
		mx = max(mx, v)
	}
	var sum float32
	for i, v := range x {
		e := exp32(v - mx)
		x[i] = e
		sum += e
	}
	inv := 1 / sum
	for i := range x {
		x[i] *= inv
	}
}

// layerNormRow sets x = LayerNorm(x + res) in place; res may be nil.
// Statistics accumulate in float64: they are a small share of the work and it
// keeps the result independent of the hidden size's summation order.
func layerNormRow(x, res []float32, ln layerNorm, eps float32) {
	n := len(x)
	if res != nil {
		res = res[:n]
		for i := range x {
			x[i] += res[i]
		}
	}
	var mean float64
	for _, v := range x {
		mean += float64(v)
	}
	mean /= float64(n)
	var variance float64
	for _, v := range x {
		dv := float64(v) - mean
		variance += dv * dv
	}
	variance /= float64(n)
	inv := float32(1 / math.Sqrt(variance+float64(eps)))
	mf := float32(mean)
	g, b := ln.g[:n], ln.b[:n]
	for i := range x {
		x[i] = (x[i]-mf)*inv*g[i] + b[i]
	}
}

func add(dst, src []float32) {
	src = src[:len(dst)]
	for i := range dst {
		dst[i] += src[i]
	}
}

func dot(a, b []float32) float32 {
	b = b[:len(a)]
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += a[i] * b[i]
		s1 += a[i+1] * b[i+1]
		s2 += a[i+2] * b[i+2]
		s3 += a[i+3] * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += a[i] * b[i]
	}
	return (s0 + s1) + (s2 + s3)
}
