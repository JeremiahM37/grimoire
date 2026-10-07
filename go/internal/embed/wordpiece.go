package embed

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// A BERT tokenizer: BertNormalizer → BertPreTokenizer → WordPiece.
//
// Ported from the HuggingFace `tokenizers` pipeline the model ships with, since
// the embedding is only reproducible if the token ids are. Config for
// potion-base-8M's tokenizer (BAAI/bge-base-en-v1.5):
//
//	normalizer:     BertNormalizer{clean_text, handle_chinese_chars, lowercase}
//	pre_tokenizer:  BertPreTokenizer
//	model:          WordPiece{unk="[UNK]", prefix="##", max_input_chars=100}
//
// strip_accents is null in the config, which HF resolves to the value of
// lowercase — so accents ARE stripped here. Getting that backwards silently
// changes the ids for every accented word rather than failing.

type WordPiece struct {
	vocab                map[string]int32
	unkToken             string
	continuingPrefix     string
	maxInputCharsPerWord int
	lowercase            bool
	stripAccents         bool
	cleanText            bool
	handleChineseChars   bool

	// special holds the added tokens matched verbatim in the raw text before
	// normalization, as HF tokenizers does; nil disables the split (see
	// SplitSpecialTokens).
	special []specialToken
	added   []specialToken
}

type specialToken struct {
	text string
	id   int32
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
	AddedTokens []struct {
		ID         int32  `json:"id"`
		Content    string `json:"content"`
		Special    bool   `json:"special"`
		Normalized bool   `json:"normalized"`
	} `json:"added_tokens"`
	Model struct {
		Type                 string           `json:"type"`
		UnkToken             string           `json:"unk_token"`
		ContinuingPrefix     string           `json:"continuing_subword_prefix"`
		MaxInputCharsPerWord int              `json:"max_input_chars_per_word"`
		Vocab                map[string]int32 `json:"vocab"`
	} `json:"model"`
}

// ParseWordPiece builds a tokenizer from a HuggingFace tokenizer.json whose
// model is WordPiece behind a BertNormalizer. Added tokens are recorded but
// not split out of the text until SplitSpecialTokens is called, which keeps
// the model2vec path byte-for-byte what it was validated as.
func ParseWordPiece(raw []byte) (*WordPiece, error) {
	var tj tokenizerJSON
	if err := json.Unmarshal(raw, &tj); err != nil {
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
	w := &WordPiece{
		vocab:                tj.Model.Vocab,
		unkToken:             tj.Model.UnkToken,
		continuingPrefix:     prefix,
		maxInputCharsPerWord: maxChars,
		lowercase:            lower,
		stripAccents:         strip,
		cleanText:            tj.Normalizer == nil || tj.Normalizer.CleanText == nil || *tj.Normalizer.CleanText,
		handleChineseChars:   tj.Normalizer == nil || tj.Normalizer.HandleChineseChars == nil || *tj.Normalizer.HandleChineseChars,
	}
	for _, at := range tj.AddedTokens {
		// a special, un-normalized added token is matched verbatim in the
		// raw input; the BERT tokenizers this targets have no other kind
		if at.Special && !at.Normalized && at.Content != "" {
			w.added = append(w.added, specialToken{text: at.Content, id: at.ID})
		}
	}
	return w, nil
}

// SplitSpecialTokens makes Encode treat added special tokens ("[SEP]",
// "[CLS]", …) appearing in the text as those tokens, as HF tokenizers does by
// default. Without it "[SEP]" in a document is tokenized as punctuation and
// letters.
func (w *WordPiece) SplitSpecialTokens() { w.special = w.added }

// normalize applies the BertNormalizer stages in order.
func (w *WordPiece) normalize(s string) string {
	if w.cleanText {
		s = cleanText(s)
	}
	if w.handleChineseChars {
		s = padChineseChars(s)
	}
	if w.lowercase {
		s = strings.ToLower(s)
	}
	if w.stripAccents {
		s = stripAccents(s)
	}
	return s
}

// cleanText drops null bytes and control characters and folds every whitespace
// variant to a plain space, matching BertNormalizer's clean_text.
func cleanText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r == 0 || r == 0xFFFD || isControl(r) {
			continue
		}
		if isWhitespace(r) {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isControl(r rune) bool {
	if r == '\t' || r == '\n' || r == '\r' {
		return false // treated as whitespace, not control
	}
	return unicode.IsControl(r) || unicode.In(r, unicode.Cf)
}

func isWhitespace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\r':
		return true
	}
	return unicode.IsSpace(r)
}

// padChineseChars surrounds CJK ideographs with spaces so each becomes its own
// token, matching handle_chinese_chars.
func padChineseChars(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isChineseChar(r) {
			b.WriteByte(' ')
			b.WriteRune(r)
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isChineseChar uses the CJK ideograph blocks the reference implementation
// lists explicitly — deliberately NOT all of unicode.Han, which would also
// catch punctuation and symbols the tokenizer treats differently.
func isChineseChar(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF,
		r >= 0x3400 && r <= 0x4DBF,
		r >= 0x20000 && r <= 0x2A6DF,
		r >= 0x2A700 && r <= 0x2B73F,
		r >= 0x2B740 && r <= 0x2B81F,
		r >= 0x2B820 && r <= 0x2CEAF,
		r >= 0xF900 && r <= 0xFAFF,
		r >= 0x2F800 && r <= 0x2FA1F:
		return true
	}
	return false
}

// stripAccents decomposes and drops combining marks (NFD, minus Mn).
func stripAccents(s string) string {
	t := transform.Chain(norm.NFD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	out, _, err := transform.String(t, s)
	if err != nil {
		return s
	}
	return out
}

// preTokenize splits on whitespace, then splits punctuation into single-rune
// tokens — BertPreTokenizer.
func preTokenize(s string) []string {
	var words []string
	for _, chunk := range strings.FieldsFunc(s, isWhitespace) {
		start := 0
		runesOf := []rune(chunk)
		for i, r := range runesOf {
			if !isPunctuation(r) {
				continue
			}
			if i > start {
				words = append(words, string(runesOf[start:i]))
			}
			words = append(words, string(r))
			start = i + 1
		}
		if start < len(runesOf) {
			words = append(words, string(runesOf[start:]))
		}
	}
	return words
}

// isPunctuation matches BERT's definition: the ASCII punctuation ranges plus
// anything Unicode categorises as punctuation. Broader than unicode.IsPunct
// alone, which misses characters like '$' and '+'.
func isPunctuation(r rune) bool {
	if (r >= '!' && r <= '/') || (r >= ':' && r <= '@') ||
		(r >= '[' && r <= '`') || (r >= '{' && r <= '~') {
		return true
	}
	return unicode.IsPunct(r)
}

// Encode returns the token ids for a string, with no special tokens added.
func (w *WordPiece) Encode(text string) []int32 {
	if len(w.special) == 0 {
		return w.encodeSegment(nil, text)
	}
	var ids []int32
	start := 0
	for i := 0; i < len(text); {
		if tok, ok := w.specialAt(text[i:]); ok {
			ids = w.encodeSegment(ids, text[start:i])
			ids = append(ids, tok.id)
			i += len(tok.text)
			start = i
			continue
		}
		i++
	}
	return w.encodeSegment(ids, text[start:])
}

func (w *WordPiece) encodeSegment(ids []int32, text string) []int32 {
	for _, word := range preTokenize(w.normalize(text)) {
		ids = append(ids, w.encodeWord(word)...)
	}
	return ids
}

// specialAt returns the longest special token that s starts with.
func (w *WordPiece) specialAt(s string) (specialToken, bool) {
	best, found := specialToken{}, false
	for _, t := range w.special {
		if len(t.text) > len(best.text) && strings.HasPrefix(s, t.text) {
			best, found = t, true
		}
	}
	return best, found
}

// TokenID looks a token up in the vocabulary.
func (w *WordPiece) TokenID(tok string) (int32, bool) {
	id, ok := w.vocab[tok]
	return id, ok
}

// encodeWord is greedy longest-match-first WordPiece. A word that cannot be
// fully segmented yields a single unk token for the WHOLE word — not per
// character.
func (w *WordPiece) encodeWord(word string) []int32 {
	chars := []rune(word)
	if len(chars) > w.maxInputCharsPerWord {
		return []int32{w.vocab[w.unkToken]}
	}
	var out []int32
	start := 0
	for start < len(chars) {
		end := len(chars)
		curID := int32(-1)
		for start < end {
			sub := string(chars[start:end])
			if start > 0 {
				sub = w.continuingPrefix + sub
			}
			if id, ok := w.vocab[sub]; ok {
				curID = id
				break
			}
			end--
		}
		if curID < 0 { // no prefix of the remainder is in the vocab
			return []int32{w.vocab[w.unkToken]}
		}
		out = append(out, curID)
		start = end
	}
	return out
}
