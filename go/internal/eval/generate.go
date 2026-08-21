package eval

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Writing the questions.
//
// Two generators, and the difference between them is not quality — it is what
// they MEASURE, which is why the set records which one wrote it and why their
// numbers must never be compared to each other.
//
//   - llm: a model reads a chunk and writes a question a person might ask,
//     using different words. That tests semantic retrieval — the thing the
//     embedder is for — and is the measurement worth having.
//   - lexical: the question is built from the chunk's most DISCRIMINATIVE
//     terms. That tests whether retrieval can find a passage from its own rare
//     vocabulary, which is a floor rather than a ceiling: a hybrid retriever
//     with a working BM25 leg should score very high, and a score that is not
//     high means something is broken rather than merely mediocre.
//
// The lexical generator exists because "run an eval" must not require an LLM.
// Grimoire's whole posture is that every AI path has a deterministic fallback;
// a measurement tool that silently needs a model would be the one place that
// stopped being true, and on a self-hosted install with no model configured it
// would simply refuse to run.

// Chunk is one indexed passage, as the generator sees it.
type Chunk struct {
	Path  string
	Index int
	Title string
	Text  string
}

// Writer turns a passage into a question. Implemented by the AI client; nil
// selects the lexical generator.
type Writer interface {
	WriteQuestion(title, chunk string) (string, error)
	Name() string
}

// Generate samples the corpus and writes a question per sampled chunk.
//
// n is the target size. Fewer questions come back when the vault is small or
// when the writer declines a passage — and declining is correct: a chunk that
// is a table of contents, a code block or three words of frontmatter has no
// question in it, and inventing one adds noise to every future comparison.
func Generate(chunks []Chunk, n int, w Writer) (Set, error) {
	if len(chunks) == 0 {
		return Set{}, fmt.Errorf("the vault has no indexed passages to ask about")
	}
	byID := make(map[string]Chunk, len(chunks))
	ids := make([]string, 0, len(chunks))
	for _, c := range chunks {
		if !usable(c.Text) {
			continue
		}
		id := passageID(c.Path, c.Index)
		byID[id] = c
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return Set{}, fmt.Errorf(
			"no passage in the vault is long enough to ask a question about")
	}
	// Oversample: some passages will be declined, and a set that came back at
	// half the requested size because of that would quietly halve the
	// resolution of every later comparison.
	want := n
	if want <= 0 {
		want = 50
	}
	sample := pick(ids, min(len(ids), want*2))

	set := Set{Created: Now().UTC().Format(time.RFC3339), Generator: "lexical"}
	if w != nil {
		set.Generator, set.Model = "llm", w.Name()
	}
	for _, id := range sample {
		if len(set.Questions) >= want {
			break
		}
		c := byID[id]
		q := ""
		if w != nil {
			out, err := w.WriteQuestion(c.Title, c.Text)
			if err != nil {
				// One passage failing must not abandon a set that takes
				// minutes to build. The generator is recorded as llm either
				// way; a mixed set would be a measurement of two things.
				continue
			}
			q = cleanQuestion(out)
		} else {
			q = LexicalQuestion(c.Title, c.Text)
		}
		if q == "" {
			continue
		}
		set.Questions = append(set.Questions, Question{
			Q: q, Path: c.Path, Chunk: c.Index, Excerpt: excerpt(c.Text),
		})
	}
	if len(set.Questions) == 0 {
		return Set{}, fmt.Errorf("no usable questions were produced from %d passages",
			len(sample))
	}
	return set, nil
}

// minChunkWords is the length below which a passage has nothing to ask about.
const minChunkWords = 25

func usable(text string) bool {
	words := 0
	for _, f := range strings.Fields(text) {
		if len(f) > 1 {
			words++
		}
	}
	return words >= minChunkWords
}

// QuestionPrompt is what an LLM writer should send. Exported so the prompt is
// visible and testable rather than buried in a client.
//
// The instruction that matters is "do not reuse the passage's distinctive
// words". Without it a model writes a question by lightly rearranging the
// sentence, BM25 matches it exactly, and the set scores 95% against every
// embedder — measuring nothing, while looking like a pass.
const QuestionPrompt = "Below is a passage from someone's notes. Write ONE question that " +
	"this passage answers and that somebody who had not read it might " +
	"plausibly ask.\n" +
	"Rules:\n" +
	"- Ask about the SPECIFIC fact in the passage, not the general topic.\n" +
	"- Do NOT reuse the passage's rare or distinctive words; paraphrase them. " +
	"A question that copies the passage's vocabulary tests nothing.\n" +
	"- If the passage states no fact worth asking about (a heading list, a " +
	"code block, boilerplate), reply exactly: SKIP\n" +
	"- Reply with the question alone, no preamble, no quotes.\n\n"

var (
	wordRE     = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9'_-]*`)
	quotedRE   = regexp.MustCompile(`^["'“”](.*)["'“”]$`)
	preambleRE = regexp.MustCompile(`(?i)^(question|q)\s*[:.\-]\s*`)
)

// cleanQuestion normalizes a model's reply and rejects a refusal.
func cleanQuestion(out string) string {
	s := strings.TrimSpace(out)
	// A reasoning model may narrate; the question is the last non-empty line.
	if lines := strings.Split(s, "\n"); len(lines) > 1 {
		for i := len(lines) - 1; i >= 0; i-- {
			if t := strings.TrimSpace(lines[i]); t != "" {
				s = t
				break
			}
		}
	}
	s = preambleRE.ReplaceAllString(s, "")
	if m := quotedRE.FindStringSubmatch(s); m != nil {
		s = strings.TrimSpace(m[1])
	}
	if s == "" || strings.EqualFold(s, "SKIP") || strings.HasPrefix(s, "SKIP") {
		return ""
	}
	// A "question" with no question in it is a model that answered instead of
	// asking, which happens often enough to be worth catching.
	if !strings.Contains(s, "?") {
		return ""
	}
	if len(s) > 300 {
		return ""
	}
	return s
}

// LexicalQuestion builds the offline probe: the passage's most distinctive
// terms, in the order they appear, prefixed by the note's title.
//
// In the passage's own words on purpose. This measures whether a passage is
// findable from its own rare vocabulary — a floor, not a proxy for the
// semantic question an LLM would write, and the set records which generator
// produced it so the two are never compared.
//
// Note that the title prefix all but gives the NOTE away, so on a lexical set
// note-recall is close to free and the chunk-level number is the one that
// carries information. That is not a flaw to fix by dropping the title: with
// it, the two numbers separate "found the document" from "found the passage",
// which is precisely the diagnostic that tells a chunking regression apart
// from an embedding one.
func LexicalQuestion(title, text string) string {
	terms := distinctiveTerms(text, 6)
	if len(terms) < 3 {
		return ""
	}
	q := strings.Join(terms, " ")
	if t := strings.TrimSpace(title); t != "" {
		q = t + ": " + q
	}
	return q + "?"
}

// commonWords are dropped when looking for what makes a passage distinctive.
// Short on purpose — a long stop list starts removing the domain words that
// ARE the signal.
var commonWords = map[string]bool{
	"the": true, "a": true, "an": true, "and": true, "or": true, "but": true,
	"of": true, "to": true, "in": true, "on": true, "at": true, "for": true,
	"is": true, "are": true, "was": true, "were": true, "be": true, "been": true,
	"it": true, "its": true, "this": true, "that": true, "these": true,
	"those": true, "with": true, "as": true, "by": true, "from": true,
	"can": true, "will": true, "would": true, "should": true, "not": true,
	"you": true, "your": true, "we": true, "our": true, "they": true,
	"have": true, "has": true, "had": true, "do": true, "does": true,
	"if": true, "when": true, "then": true, "so": true, "than": true,
}

// distinctiveTerms picks the least ordinary words in a passage: long, rare
// within the passage's own text, and not stopwords. Order of first appearance
// is preserved so the result reads as a phrase rather than a bag.
func distinctiveTerms(text string, n int) []string {
	counts := map[string]int{}
	var order []string
	for _, w := range wordRE.FindAllString(text, -1) {
		lw := strings.ToLower(w)
		if commonWords[lw] || len(lw) < 4 {
			continue
		}
		if counts[lw] == 0 {
			order = append(order, lw)
		}
		counts[lw]++
	}
	if len(order) == 0 {
		return nil
	}
	ranked := append([]string(nil), order...)
	pos := map[string]int{}
	for i, w := range order {
		pos[w] = i
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		// Rarer first; longer breaks the tie, since a long word is more
		// discriminating than a short one at the same frequency.
		if counts[ranked[i]] != counts[ranked[j]] {
			return counts[ranked[i]] < counts[ranked[j]]
		}
		if len(ranked[i]) != len(ranked[j]) {
			return len(ranked[i]) > len(ranked[j])
		}
		return pos[ranked[i]] < pos[ranked[j]]
	})
	if len(ranked) > n {
		ranked = ranked[:n]
	}
	sort.SliceStable(ranked, func(i, j int) bool { return pos[ranked[i]] < pos[ranked[j]] })
	return ranked
}

func excerpt(text string) string {
	s := strings.Join(strings.Fields(text), " ")
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}
