package distill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// Examples are generated from the selected facts with fixed templates. No model
// writes them: a model in the loop would make the training data depend on
// whatever that model happened to say, and the held-out set would then be
// measuring the generator as much as the student.
//
// Two template sets are used and they never share a question string. The
// training set uses one; the held-out set uses the other, over the same facts,
// so the eval asks whether the baked fact can be recalled under phrasings the
// model was not trained on rather than whether it can repeat its training text.

// SystemPrompt is the fixed system turn for every example, in both files.
const SystemPrompt = "You answer from a personal memory store. Answer only what the store records. If it does not record the answer, say you do not have it."

// AbstainAnswer is the reply for a question about something the store does not
// hold. Training on it is what teaches the model to decline rather than invent.
const AbstainAnswer = "I do not have that in my memory, so I cannot say."

// Template IDs. The train and held-out sets must not share any of these, and
// the test checks the rendered question strings as well.
const (
	TrainQAFact      = "train-qa-fact"
	TrainBlank       = "train-complete-blank"
	TrainAbstain     = "train-qa-abstain"
	HeldRecallFact   = "heldout-recall-fact"
	HeldFillBlank    = "heldout-fill-blank"
	HeldRecallAbsent = "heldout-recall-abstain"
)

var templateText = map[string]string{
	TrainQAFact:      "What does your memory say about %s?",
	TrainBlank:       "Complete this memory entry: %s %s ___",
	TrainAbstain:     "What does your memory say about %s?",
	HeldRecallFact:   "Recall what is on file about %s.",
	HeldFillBlank:    "Fill in from memory: %s %s ?",
	HeldRecallAbsent: "Recall what is on file about %s.",
}

// Message is one turn of a chat-format example.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Example is one JSONL line. Messages is the chat format trainers read; the
// other fields are audit metadata a trainer ignores.
type Example struct {
	ID       string    `json:"id"`
	Template string    `json:"template"`
	Kind     string    `json:"kind"`
	Messages []Message `json:"messages"`
}

// Built is the whole output before it is written.
type Built struct {
	Train   []Example
	Eval    []Example
	Skipped int // selected facts that yielded no subject and so no example
}

// Build turns a selection into training and held-out examples. corpus is every
// fact text the store holds, selected or not; an abstain subject is only used
// if it appears in none of them, so a "the store does not know this" example is
// never a question about something the store does know.
func Build(sel Result, corpus []string) Built {
	var b Built
	for _, c := range sel.Selected {
		text := strings.TrimSpace(c.Entry.Text)
		id := exampleID(c)
		if subj, pred, val, ok := memory.Attribute(text); ok {
			b.Train = append(b.Train,
				example(id+"-a", TrainQAFact, "fact", fmt.Sprintf(templateText[TrainQAFact], subj), text),
				example(id+"-b", TrainBlank, "fact", fmt.Sprintf(templateText[TrainBlank], subj, pred), val))
			b.Eval = append(b.Eval,
				example(id+"-a", HeldRecallFact, "fact", fmt.Sprintf(templateText[HeldRecallFact], subj), text),
				example(id+"-b", HeldFillBlank, "fact", fmt.Sprintf(templateText[HeldFillBlank], subj, pred), val))
			continue
		}
		key := slotKey(text)
		if key == "" {
			b.Skipped++
			continue
		}
		b.Train = append(b.Train, example(id+"-a", TrainQAFact, "fact", fmt.Sprintf(templateText[TrainQAFact], key), text))
		b.Eval = append(b.Eval, example(id+"-a", HeldRecallFact, "fact", fmt.Sprintf(templateText[HeldRecallFact], key), text))
	}

	// One abstain example per selected fact, in each split, about subjects the
	// store does not hold. Train and eval draw from disjoint runs of the
	// generator, so an eval subject is never a training subject.
	n := len(sel.Selected)
	names := absentSubjects(corpus, 2*n)
	for i := 0; i < n && i < len(names); i++ {
		b.Train = append(b.Train, example(fmt.Sprintf("abstain-train-%04d", i+1), TrainAbstain, "abstain",
			fmt.Sprintf(templateText[TrainAbstain], names[i]), AbstainAnswer))
	}
	for i := 0; i < n && n+i < len(names); i++ {
		b.Eval = append(b.Eval, example(fmt.Sprintf("abstain-eval-%04d", i+1), HeldRecallAbsent, "abstain",
			fmt.Sprintf(templateText[HeldRecallAbsent], names[n+i]), AbstainAnswer))
	}
	return b
}

// exampleID names an example by its fact's note and id. The id is stable across
// reruns and changes only when the fact is rewritten, which is the signal the
// manifest diff is after.
func exampleID(c Candidate) string {
	sum := sha256.Sum256([]byte(c.Note + "\x00" + c.Entry.ID))
	return "fact-" + hex.EncodeToString(sum[:])[:12]
}

func example(id, template, kind, question, answer string) Example {
	return Example{
		ID:       id,
		Template: template,
		Kind:     kind,
		Messages: []Message{
			{Role: "system", Content: SystemPrompt},
			{Role: "user", Content: question},
			{Role: "assistant", Content: answer},
		},
	}
}

// slotKey is the subject of a fact that has no subject-predicate-value shape.
// It is the first six words of the normalized text. That is positional, so it
// is a weak key: a fact that does not parse gets a weaker question than one
// that does, and the manifest counts these so a reader can see how many.
func slotKey(text string) string {
	words := strings.Fields(memory.Normalize(text))
	if len(words) > 6 {
		words = words[:6]
	}
	return strings.Join(words, " ")
}

var (
	absentAdjectives = []string{"Quorvane", "Ilmstrand", "Pezzotin", "Varnhollow", "Kestrume", "Dorwynn", "Mirrenhal", "Tavesque", "Blenwick", "Osterlin"}
	absentNouns      = []string{"backup schedule", "project lead", "office printer", "deploy window", "api quota", "lab rack", "tea order", "garden plan"}
)

// absentSubjects returns n subjects that appear in none of the corpus texts.
// Deterministic: the k-th candidate is always the same string.
func absentSubjects(corpus []string, n int) []string {
	lower := make([]string, len(corpus))
	for i, s := range corpus {
		lower[i] = strings.ToLower(s)
	}
	var out []string
	span := len(absentAdjectives) * len(absentNouns)
	for k := 0; len(out) < n && k < 100*n+span; k++ {
		name := fmt.Sprintf("the %s %s %d",
			absentAdjectives[k%len(absentAdjectives)],
			absentNouns[(k/len(absentAdjectives))%len(absentNouns)],
			k/span+1)
		if mentioned(lower, strings.ToLower(name)) {
			continue
		}
		out = append(out, name)
	}
	return out
}

func mentioned(lower []string, needle string) bool {
	for _, s := range lower {
		if strings.Contains(s, needle) {
			return true
		}
	}
	return false
}

// marshalLines renders examples as JSONL. encoding/json writes struct fields in
// declaration order, so the bytes depend only on the examples.
func marshalLines(exs []Example) ([]byte, error) {
	var out []byte
	for _, ex := range exs {
		line, err := json.Marshal(ex)
		if err != nil {
			return nil, err
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	return out, nil
}
