package skillgate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/anchor"
)

// Written procedures are the write path for a skill with a spec: an agent that
// just finished a multi-step task writes its steps, and the checks that hold
// before and after them, as anchors. They are stored with the same note format
// `skills accept` uses, so `check` and `export` treat them like any other skill.

// WrittenSource is the metadata.skill_source value of a procedure written by
// the agent route. Only a note carrying it may be replaced by that route.
const WrittenSource = "written"

// Procedure is the content of a written procedure, after validation.
type Procedure struct {
	Name    string
	When    string
	Steps   []string
	Pre     []anchor.Anchor
	Post    []anchor.Anchor
	Host    string // the machine the checks are about; empty for this one
	Topic   string
	Agent   string
	Session string // provenance: the session the procedure was learned in
}

// Spec is the structured form of the procedure: its trigger, the anchors as
// predicates, the steps and the session it came from.
func (p Procedure) Spec() Spec {
	s := Spec{Trigger: p.When, Steps: p.Steps}
	for _, a := range p.Pre {
		s.Preconditions = append(s.Preconditions, AnchorPredicate(a))
	}
	for _, a := range p.Post {
		s.Postconditions = append(s.Postconditions, AnchorPredicate(a))
	}
	if p.Session != "" {
		s.Sessions = []string{p.Session}
	}
	return s
}

// Note renders the procedure as the text of a skill note: frontmatter that
// names it and its trigger, the marker that says the route wrote it, and the
// numbered steps in the body. The spec lines are added by Stamp.
func (p Procedure) Note() (string, error) {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + yamlQuote(p.Name) + "\n")
	b.WriteString("description: " + yamlQuote(p.When) + "\n")
	b.WriteString("metadata:\n  kind: procedure\n  skill_source: " + WrittenSource + "\n")
	if cues := cueText(p.When); cues != "" {
		b.WriteString("  cues: " + yamlQuote(cues) + "\n")
	}
	if p.Agent != "" {
		b.WriteString("  agent: " + yamlQuote(p.Agent) + "\n")
	}
	if p.Topic != "" {
		b.WriteString("  topic: " + yamlQuote(p.Topic) + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString("# " + p.Name + "\n\n")
	b.WriteString("Use when: " + p.When + "\n\n")
	b.WriteString("Steps, as run (`<name>` marks a part to fill in before use):\n\n")
	for i, st := range p.Steps {
		fmt.Fprintf(&b, "%d. %s\n", i+1, st)
	}
	if len(p.Pre) > 0 {
		b.WriteString("\nChecked before starting:\n\n")
		for _, a := range p.Pre {
			b.WriteString("- " + describeAnchor(a) + "\n")
		}
	}
	if len(p.Post) > 0 {
		b.WriteString("\nChecked once it has worked:\n\n")
		for _, a := range p.Post {
			b.WriteString("- " + describeAnchor(a) + "\n")
		}
	}
	draft := b.String()
	return Stamp(draft, p.Spec(), nil)
}

// describeAnchor names what an anchor checks, without its literal text: the
// type and the thing it is about, which is what a reader needs.
func describeAnchor(a anchor.Anchor) string {
	for _, k := range []string{"path", "unit", "port", "url", "bin", "repo"} {
		if v := a.Args[k]; v != "" {
			return a.Type + " " + v
		}
	}
	return a.Type
}

// cueText is the trigger as a cue: list separators would split it.
func cueText(when string) string {
	return strings.TrimSpace(strings.NewReplacer(";", " ", ",", " ", "|", " ", "\n", " ").Replace(when))
}

// SaveNote writes a skill note into the store. With replace false the file must
// not exist (what `skills accept` always did). With replace true an existing
// file is replaced atomically, so a reader never sees half a note. It returns
// the path written.
func SaveNote(dir, file, text string, replace bool) (string, error) {
	path := filepath.Join(dir, file)
	if !replace {
		fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return "", fmt.Errorf("%s already exists; nothing written", path)
		}
		_, werr := fh.WriteString(text)
		if cerr := fh.Close(); werr != nil || cerr != nil {
			return "", fmt.Errorf("could not write %s", path)
		}
		return path, nil
	}
	tmp := path + ".tmp-" + randomSuffix()
	if err := os.WriteFile(tmp, []byte(text), 0o644); err != nil {
		return "", fmt.Errorf("could not write %s", path)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("could not replace %s", path)
	}
	return path, nil
}

func randomSuffix() string {
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		return "x"
	}
	return hex.EncodeToString(b)
}

// ErrNoStore means the store directory does not exist yet.
var ErrNoStore = errors.New("memory store does not exist")

// CheckStore is the store directory the writer needs; a missing one is an
// error, not something the writer creates.
func CheckStore(dir string) error {
	fi, err := os.Stat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNoStore
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return nil
}
