// Package embed produces the vectors retrieval ranks on.
//
// Port of the embedding half of server/ai.py. Two backends live here: the
// dependency-free hash embedder (the always-available fallback) and model2vec,
// a static embedding model — a tokenizer plus a lookup table, mean-pooled.
//
// This is the highest-risk surface in the port. Vectors that differ from
// Python's don't fail loudly; they quietly move retrieval quality, which would
// mean the published LoCoMo and LongMemEval numbers no longer describe the
// shipped binary. Everything here is validated component-by-component against
// fixtures rather than trusted.
package embed

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
)

// Model2Vec is a static embedding model: token ids index a frozen matrix, and
// the sentence vector is the mean of its token vectors, L2-normalized.
type Model2Vec struct {
	Name      string
	tokenizer *wordPiece
	embedding []float32 // row-major, rows*dim
	rows      int
	dim       int
	normalize bool

	unkTokenID        int32
	medianTokenLength int
	maxLength         int
}

// tokenizerJSON is the subset of tokenizer.json this needs.
type tokenizerJSON struct {
	Normalizer *struct {
		Type               string `json:"type"`
		CleanText          *bool  `json:"clean_text"`
		HandleChineseChars *bool  `json:"handle_chinese_chars"`
		StripAccents       *bool  `json:"strip_accents"`
		Lowercase          *bool  `json:"lowercase"`
	} `json:"normalizer"`
	Model struct {
		Type                 string           `json:"type"`
		UnkToken             string           `json:"unk_token"`
		ContinuingPrefix     string           `json:"continuing_subword_prefix"`
		MaxInputCharsPerWord int              `json:"max_input_chars_per_word"`
		Vocab                map[string]int32 `json:"vocab"`
	} `json:"model"`
}

// LoadModel2Vec reads a model2vec directory (tokenizer.json + model.safetensors),
// as produced by a HuggingFace snapshot download.
func LoadModel2Vec(dir, name string) (*Model2Vec, error) {
	tkRaw, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("reading tokenizer: %w", err)
	}
	var tj tokenizerJSON
	if err := json.Unmarshal(tkRaw, &tj); err != nil {
		return nil, fmt.Errorf("parsing tokenizer: %w", err)
	}
	if tj.Model.Type != "WordPiece" {
		return nil, fmt.Errorf("unsupported tokenizer model %q (only WordPiece)", tj.Model.Type)
	}

	lower := true
	if tj.Normalizer != nil && tj.Normalizer.Lowercase != nil {
		lower = *tj.Normalizer.Lowercase
	}
	// strip_accents is null in this config; HuggingFace resolves that to the
	// value of lowercase, so it must not default to false.
	strip := lower
	if tj.Normalizer != nil && tj.Normalizer.StripAccents != nil {
		strip = *tj.Normalizer.StripAccents
	}
	prefix := tj.Model.ContinuingPrefix
	if prefix == "" {
		prefix = "##"
	}
	maxChars := tj.Model.MaxInputCharsPerWord
	if maxChars == 0 {
		maxChars = 100
	}

	tk := &wordPiece{
		vocab:                tj.Model.Vocab,
		unkToken:             tj.Model.UnkToken,
		continuingPrefix:     prefix,
		maxInputCharsPerWord: maxChars,
		lowercase:            lower,
		stripAccents:         strip,
		cleanText:            tj.Normalizer == nil || tj.Normalizer.CleanText == nil || *tj.Normalizer.CleanText,
		handleChineseChars:   tj.Normalizer == nil || tj.Normalizer.HandleChineseChars == nil || *tj.Normalizer.HandleChineseChars,
	}

	emb, rows, dim, err := loadSafetensors(filepath.Join(dir, "model.safetensors"))
	if err != nil {
		return nil, err
	}
	unk, ok := tj.Model.Vocab[tj.Model.UnkToken]
	if !ok {
		unk = -1
	}
	return &Model2Vec{
		Name: name, tokenizer: tk, embedding: emb, rows: rows, dim: dim,
		normalize: true, unkTokenID: unk,
		medianTokenLength: 6, maxLength: 512,
	}, nil
}

// Dim reports the vector width.
func (m *Model2Vec) Dim() int { return m.dim }

// Signature identifies the backend. Vectors from different backends are not
// comparable, so the index re-embeds when this changes.
func (m *Model2Vec) Signature() string { return "model2vec:" + m.Name }

// Embed encodes a batch of texts.
func (m *Model2Vec) Embed(texts []string) [][]float32 {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		out[i] = m.embedOne(t)
	}
	return out
}

// tokenize mirrors StaticModel.tokenize: pre-truncate by characters, encode,
// drop unknown tokens, then truncate to max_length tokens.
//
// Dropping unk is not cosmetic — an unknown word contributes nothing to the
// mean rather than dragging it toward the unk vector, so a vector computed
// without this step is subtly wrong for any text containing an emoji or a rare
// word, in a way no error would ever surface.
func (m *Model2Vec) tokenize(text string) []int32 {
	// Character-level pre-truncation, exactly as model2vec does before
	// tokenizing: max_length tokens * the model's median token length.
	if m.maxLength > 0 {
		limit := m.maxLength * m.medianTokenLength
		if r := []rune(text); len(r) > limit {
			text = string(r[:limit])
		}
	}
	ids := m.tokenizer.Encode(text)

	kept := ids[:0]
	for _, id := range ids {
		if id != m.unkTokenID {
			kept = append(kept, id)
		}
	}
	ids = kept
	if m.maxLength > 0 && len(ids) > m.maxLength {
		ids = ids[:m.maxLength]
	}
	return ids
}

func (m *Model2Vec) embedOne(text string) []float32 {
	ids := m.tokenize(text)

	if len(ids) == 0 {
		// an empty sentence embeds as zeros, and is NOT normalized
		return make([]float32, m.dim)
	}
	// gather the token rows, then mean-pool them exactly as numpy would
	rows := make([][]float32, 0, len(ids))
	for _, id := range ids {
		if int(id) >= m.rows || id < 0 {
			continue
		}
		rows = append(rows, m.embedding[int(id)*m.dim:(int(id)+1)*m.dim])
	}
	if len(rows) == 0 {
		return make([]float32, m.dim)
	}
	vec := meanColumns(rows, m.dim)
	if m.normalize {
		l2NormalizeF32(vec)
	}
	return vec
}

// ---------------------------------------------------------------- safetensors

// loadSafetensors reads the single 2-D float32 tensor a model2vec file holds.
func loadSafetensors(path string) (data []float32, rows, dim int, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, 0, fmt.Errorf("reading weights: %w", err)
	}
	if len(raw) < 8 {
		return nil, 0, 0, fmt.Errorf("safetensors file too short")
	}
	headerLen := binary.LittleEndian.Uint64(raw[:8])
	if uint64(len(raw)) < 8+headerLen {
		return nil, 0, 0, fmt.Errorf("safetensors header overruns the file")
	}
	var header map[string]json.RawMessage
	if err := json.Unmarshal(raw[8:8+headerLen], &header); err != nil {
		return nil, 0, 0, fmt.Errorf("parsing safetensors header: %w", err)
	}
	type entry struct {
		DType   string   `json:"dtype"`
		Shape   []int    `json:"shape"`
		Offsets []uint64 `json:"data_offsets"`
	}
	for name, rawEntry := range header {
		if name == "__metadata__" {
			continue
		}
		var e entry
		if err := json.Unmarshal(rawEntry, &e); err != nil {
			continue
		}
		if len(e.Shape) != 2 || len(e.Offsets) != 2 {
			continue
		}
		if e.DType != "F32" {
			return nil, 0, 0, fmt.Errorf("tensor %q has dtype %s, expected F32", name, e.DType)
		}
		start := 8 + headerLen + e.Offsets[0]
		end := 8 + headerLen + e.Offsets[1]
		if end > uint64(len(raw)) || start > end {
			return nil, 0, 0, fmt.Errorf("tensor %q offsets out of range", name)
		}
		rows, dim = e.Shape[0], e.Shape[1]
		blob := raw[start:end]
		if len(blob) != rows*dim*4 {
			return nil, 0, 0, fmt.Errorf("tensor %q is %d bytes, expected %d",
				name, len(blob), rows*dim*4)
		}
		data = make([]float32, rows*dim)
		for i := range data {
			data[i] = math.Float32frombits(binary.LittleEndian.Uint32(blob[i*4:]))
		}
		return data, rows, dim, nil
	}
	return nil, 0, 0, fmt.Errorf("no 2-D tensor found in %s", path)
}
