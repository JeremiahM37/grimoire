package rerank

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
)

// pairTokenizer produces BERT sentence-pair inputs,
//
//	[CLS] query [SEP] document [SEP]
//
// with token_type_ids 0 for the first segment (both specials included) and 1
// for the document and its closing [SEP] — the TemplateProcessing pair
// template the model's tokenizer.json declares.
type pairTokenizer struct {
	wp            *embed.WordPiece
	cls, sep, pad int32
}

func loadPairTokenizer(dir string) (*pairTokenizer, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, fmt.Errorf("reading tokenizer: %w", err)
	}
	wp, err := embed.ParseWordPiece(raw)
	if err != nil {
		return nil, err
	}
	wp.SplitSpecialTokens()
	t := &pairTokenizer{wp: wp}
	for _, s := range []struct {
		tok string
		id  *int32
	}{{"[CLS]", &t.cls}, {"[SEP]", &t.sep}, {"[PAD]", &t.pad}} {
		id, ok := wp.TokenID(s.tok)
		if !ok {
			return nil, fmt.Errorf("tokenizer has no %s token", s.tok)
		}
		*s.id = id
	}
	return t, nil
}

// pairSpecials is how many special tokens a pair adds.
const pairSpecials = 3

// encodePair tokenizes a pair and truncates it to at most maxLen tokens,
// specials included.
func (t *pairTokenizer) encodePair(query, doc string, maxLen int) (ids, types []int32) {
	a := t.wp.Encode(query)
	b := t.wp.Encode(doc)
	na, nb := truncateLongestFirst(len(a), len(b), maxLen-pairSpecials)

	if doc == "" {
		// The reference tokenizer call treats an empty second text as no
		// pair at all and encodes "[CLS] query [SEP]" — matched so an empty
		// document scores the same here as there.
		na = min(len(a), maxLen-2)
		ids = make([]int32, 0, na+2)
		ids = append(ids, t.cls)
		ids = append(ids, a[:na]...)
		ids = append(ids, t.sep)
		return ids, make([]int32, len(ids))
	}
	a, b = a[:na], b[:nb]

	n := len(a) + len(b) + pairSpecials
	ids = make([]int32, 0, n)
	types = make([]int32, n)
	ids = append(ids, t.cls)
	ids = append(ids, a...)
	ids = append(ids, t.sep)
	ids = append(ids, b...)
	ids = append(ids, t.sep)
	for i := len(a) + 2; i < n; i++ {
		types[i] = 1
	}
	return ids, types
}

// truncateLongestFirst returns how many tokens of each sequence to keep so the
// pair fits in target, using the HF tokenizers "longest_first" rule: the
// shorter sequence is kept whole when the longer one can absorb the cut,
// otherwise the budget is split evenly with any odd token going to the longer
// sequence (the second, on a tie). This is a closed form, not the
// one-token-at-a-time loop of the older Python tokenizers, and the two differ
// on ties — the fast tokenizer is the reference here.
func truncateLongestFirst(na, nb, target int) (int, int) {
	if target < 0 {
		target = 0
	}
	if na+nb <= target {
		return na, nb
	}
	short := min(na, nb)
	keepShort, keepLong := short, max(short, target-short)
	if short > target || keepShort+keepLong > target {
		keepShort = target / 2
		keepLong = keepShort + target%2
	}
	if na > nb {
		return min(na, keepLong), min(nb, keepShort)
	}
	return min(na, keepShort), min(nb, keepLong)
}
