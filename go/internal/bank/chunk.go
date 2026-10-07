package bank

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

// DefaultChunkSize is the largest chunk, in characters, one extraction call
// sees. Large enough that a conversation turn and the turns around it travel
// together; small enough that the extraction reply fits any output budget.
const DefaultChunkSize = 3000

// Chunk is one piece of a retained document.
type Chunk struct {
	Index int
	Text  string
	Hash  string
}

// HashChunk is a chunk's identity for delta retain.
func HashChunk(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// Chunks cuts text into chunks of at most maxChars characters.
//
// The algorithm has to be deterministic and IDEMPOTENT: re-chunking any chunk
// it produced returns that chunk unchanged. Delta retain depends on it — a
// re-retained document is re-chunked and only chunks whose hash changed are
// re-extracted — and so does the guarantee that a chunk maps to exactly one
// extraction call.
//
// Dispatch, in order:
//   - text that already fits is returned whole and unmodified;
//   - a JSON array of objects is a conversation, packed turn by turn;
//   - one JSON object is kept whole if it fits the structured limit;
//   - JSON Lines are packed line by line;
//   - anything else is split on the most structural separator present —
//     paragraphs, then lines, then sentences, clauses, words, characters —
//     keeping each separator at the START of the piece that follows it.
func Chunks(text string, maxChars int) []Chunk {
	if maxChars <= 0 {
		maxChars = DefaultChunkSize
	}
	parts := chunkText(text, maxChars, maxChars)
	out := make([]Chunk, 0, len(parts))
	for _, p := range parts {
		out = append(out, Chunk{Index: len(out), Text: p, Hash: HashChunk(p)})
	}
	return out
}

func runeLen(s string) int { return utf8.RuneCountInString(s) }

func chunkText(text string, maxChars, structured int) []string {
	if runeLen(text) <= maxChars {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []string{text}
	}
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "[") {
		var turns []json.RawMessage
		if json.Unmarshal([]byte(trimmed), &turns) == nil && allObjects(turns) && len(turns) > 0 {
			return conversationChunks(turns, maxChars, structured)
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(trimmed), &obj) == nil && runeLen(text) <= structured {
			return []string{text}
		}
	}
	if lines, ok := jsonLines(text); ok {
		return packLines(lines, maxChars, structured)
	}
	return recursiveSplit(text, maxChars, separators)
}

func allObjects(xs []json.RawMessage) bool {
	for _, x := range xs {
		b := bytes.TrimSpace(x)
		if len(b) == 0 || b[0] != '{' {
			return false
		}
	}
	return true
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return string(raw)
	}
	return buf.String()
}

// conversationChunks packs turns greedily into JSON arrays. A turn too large
// for any chunk is flushed alone and its JSON text split as prose.
func conversationChunks(turns []json.RawMessage, maxChars, structured int) []string {
	var out, cur []string
	size := 2 // "[]"
	flush := func() {
		if len(cur) > 0 {
			out = append(out, "["+strings.Join(cur, ",")+"]")
		}
		cur, size = nil, 2
	}
	limit := min(structured, maxChars)
	for _, raw := range turns {
		t := compact(raw)
		n := runeLen(t) + 1
		if runeLen(t)+2 > limit {
			flush()
			out = append(out, recursiveSplit(t, limit, separators)...)
			continue
		}
		if size+n > maxChars && len(cur) > 0 {
			flush()
		}
		cur = append(cur, t)
		size += n
	}
	flush()
	return out
}

func jsonLines(text string) ([]string, bool) {
	var lines []string
	for _, ln := range strings.Split(text, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		var obj map[string]json.RawMessage
		if json.Unmarshal([]byte(ln), &obj) != nil {
			return nil, false
		}
		lines = append(lines, ln)
	}
	return lines, len(lines) >= 2
}

func packLines(lines []string, maxChars, structured int) []string {
	var out, cur []string
	size := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.Join(cur, "\n"))
		}
		cur, size = nil, 0
	}
	limit := min(structured, maxChars)
	for _, ln := range lines {
		n := runeLen(ln) + 1
		if runeLen(ln) > limit {
			flush()
			out = append(out, recursiveSplit(ln, limit, separators)...)
			continue
		}
		if size+n > maxChars && len(cur) > 0 {
			flush()
		}
		cur = append(cur, ln)
		size += n
	}
	flush()
	return out
}

// separators, most structural first. The empty separator splits into
// characters and is the floor every other split eventually reaches.
var separators = []string{"\n\n", "\n", ". ", "! ", "? ", "; ", ", ", " ", ""}

// recursiveSplit is the separator-cascade splitter. Its contract, which the
// tests pin:
//   - split on the first separator present, keeping it at the start of the
//     following piece;
//   - pack pieces shorter than maxChars greedily while the packed length stays
//     within maxChars;
//   - a piece of maxChars or more flushes the buffer and is split again with
//     the remaining separators, or emitted whole if none remain;
//   - every packed chunk is whitespace-stripped and empty ones dropped.
func recursiveSplit(text string, maxChars int, seps []string) []string {
	sep := seps[len(seps)-1]
	var rest []string
	for i, s := range seps {
		if s == "" {
			sep = s
			break
		}
		if strings.Contains(text, s) {
			sep, rest = s, seps[i+1:]
			break
		}
	}
	pieces := splitKeepStart(text, sep)
	var out, good []string
	for _, p := range pieces {
		if runeLen(p) < maxChars {
			good = append(good, p)
			continue
		}
		if len(good) > 0 {
			out = append(out, mergePieces(good, maxChars)...)
			good = nil
		}
		if len(rest) == 0 {
			out = append(out, p)
		} else {
			out = append(out, recursiveSplit(p, maxChars, rest)...)
		}
	}
	if len(good) > 0 {
		out = append(out, mergePieces(good, maxChars)...)
	}
	return out
}

func splitKeepStart(text, sep string) []string {
	if sep == "" {
		out := make([]string, 0, len(text))
		for _, r := range text {
			out = append(out, string(r))
		}
		return out
	}
	parts := strings.Split(text, sep)
	out := make([]string, 0, len(parts))
	if parts[0] != "" {
		out = append(out, parts[0])
	}
	for _, p := range parts[1:] {
		out = append(out, sep+p)
	}
	return out
}

func mergePieces(pieces []string, maxChars int) []string {
	var out []string
	var cur strings.Builder
	total := 0
	emit := func() {
		if s := strings.TrimSpace(cur.String()); s != "" {
			out = append(out, s)
		}
		cur.Reset()
		total = 0
	}
	for _, p := range pieces {
		n := runeLen(p)
		if total+n > maxChars && total > 0 {
			emit()
		}
		cur.WriteString(p)
		total += n
	}
	emit()
	return out
}

// renderForModel turns a JSON conversation chunk into "speaker: text" lines.
// A model reads a transcript more reliably than JSON punctuation, and it costs
// fewer tokens; anything that is not a conversation is passed through.
func renderForModel(chunk string) string {
	t := strings.TrimSpace(chunk)
	if !strings.HasPrefix(t, "[") {
		return chunk
	}
	var turns []map[string]any
	if json.Unmarshal([]byte(t), &turns) != nil || len(turns) == 0 {
		return chunk
	}
	var b strings.Builder
	for _, turn := range turns {
		speaker := firstString(turn, "speaker", "role", "name", "author", "from")
		text := firstString(turn, "content", "text", "message", "body", "value")
		if text == "" {
			raw, _ := json.Marshal(turn)
			text = string(raw)
		}
		when := firstString(turn, "timestamp", "time", "date", "created_at")
		if speaker == "" {
			speaker = "unknown"
		}
		b.WriteString(speaker)
		if when != "" {
			b.WriteString(" [" + when + "]")
		}
		b.WriteString(": " + strings.TrimSpace(text) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			switch t := v.(type) {
			case string:
				if t != "" {
					return t
				}
			case float64, bool:
				b, _ := json.Marshal(t)
				return string(b)
			}
		}
	}
	return ""
}
