package distill

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// File names inside the output directory. The manifest is the index: a rerun
// on another day or after a vault change is diffed by its "facts" list.
const (
	TrainFile      = "train.jsonl"
	EvalFile       = "eval.jsonl"
	ExclusionsFile = "exclusions.json"
	ManifestFile   = "manifest.json"
	ManifestFormat = "grimoire-distill"
	ManifestVer    = 1
)

// Manifest describes one distillation run. Every field is derived from the
// inputs, so two runs over the same facts on the same date are byte-identical.
type Manifest struct {
	Format        string           `json:"format"`
	Version       int              `json:"version"`
	Date          string           `json:"date"`
	Options       ManifestOptions  `json:"options"`
	Counts        ManifestCounts   `json:"counts"`
	Templates     ManifestTemplate `json:"templates"`
	Files         []FileDigest     `json:"files"`
	Facts         []FactDigest     `json:"facts"`
	ContentSHA256 string           `json:"content_sha256"`
	Limits        []string         `json:"limits"`
}

// ManifestOptions are the thresholds used. They are part of the manifest so a
// diff between two runs that used different thresholds is visible as such.
type ManifestOptions struct {
	MinAgeDays int `json:"min_age_days"`
	MinUses    int `json:"min_uses"`
}

// ManifestCounts tallies the run. ExcludedByReason sums to Excluded.
type ManifestCounts struct {
	Candidates       int            `json:"candidates"`
	Selected         int            `json:"selected"`
	Excluded         int            `json:"excluded"`
	ExcludedByReason map[string]int `json:"excluded_by_reason"`
	SkippedNoSubject int            `json:"skipped_no_subject"`
	TrainExamples    int            `json:"train_examples"`
	TrainAbstain     int            `json:"train_abstain"`
	EvalExamples     int            `json:"eval_examples"`
	EvalAbstain      int            `json:"eval_abstain"`
}

// ManifestTemplate lists the template IDs each split uses. The held-out check
// is that these two lists do not intersect.
type ManifestTemplate struct {
	Train []string `json:"train"`
	Eval  []string `json:"eval"`
}

// FileDigest is one output file's hash and line count.
type FileDigest struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Lines  int    `json:"lines"`
}

// FactDigest identifies a selected fact without carrying its text. The hash is
// of the text, so a fact whose wording changed shows up in a diff even when its
// id did not.
type FactDigest struct {
	ID     string `json:"id"`
	Note   string `json:"note"`
	SHA256 string `json:"sha256"`
}

// Limits are stated in the manifest itself, because a bare file of examples
// travels without the document that explains what it is and is not.
var Limits = []string{
	"exports data only; trains nothing",
	"selection is a snapshot: a baked fact is not re-checked when the vault changes",
	"abstain examples teach declining on subjects absent from the store, not calibrated uncertainty",
}

// Run selects, builds and writes. It never modifies the memory it reads from.
func Run(cands []Candidate, opt Options, outDir string) (Manifest, error) {
	sel, err := Select(cands, opt)
	if err != nil {
		return Manifest{}, err
	}
	corpus := make([]string, 0, len(cands))
	for _, c := range cands {
		corpus = append(corpus, c.Entry.Text)
	}
	b := Build(sel, corpus)

	train, err := marshalLines(b.Train)
	if err != nil {
		return Manifest{}, err
	}
	eval, err := marshalLines(b.Eval)
	if err != nil {
		return Manifest{}, err
	}
	exclusions, err := json.MarshalIndent(sel.Excluded, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	exclusions = append(exclusions, '\n')

	m := Manifest{
		Format:  ManifestFormat,
		Version: ManifestVer,
		Date:    opt.Now.Format("2006-01-02"),
		Options: ManifestOptions{MinAgeDays: opt.MinAgeDays, MinUses: opt.MinUses},
		Counts: ManifestCounts{
			Candidates:       len(cands),
			Selected:         len(sel.Selected),
			Excluded:         len(sel.Excluded),
			ExcludedByReason: nonNilCounts(sel.Counts),
			SkippedNoSubject: b.Skipped,
			TrainExamples:    len(b.Train) - countKind(b.Train, "abstain"),
			TrainAbstain:     countKind(b.Train, "abstain"),
			EvalExamples:     len(b.Eval) - countKind(b.Eval, "abstain"),
			EvalAbstain:      countKind(b.Eval, "abstain"),
		},
		Templates: ManifestTemplate{Train: templatesOf(b.Train), Eval: templatesOf(b.Eval)},
		Limits:    Limits,
	}
	for _, c := range sel.Selected {
		sum := sha256.Sum256([]byte(c.Entry.Text))
		m.Facts = append(m.Facts, FactDigest{ID: c.Entry.ID, Note: c.Note, SHA256: hex.EncodeToString(sum[:])})
	}
	m.Files = []FileDigest{
		digest(TrainFile, train, len(b.Train)),
		digest(EvalFile, eval, len(b.Eval)),
		digest(ExclusionsFile, exclusions, len(sel.Excluded)),
	}
	content := sha256.New()
	content.Write(train)
	content.Write(eval)
	m.ContentSHA256 = hex.EncodeToString(content.Sum(nil))

	manifest, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Manifest{}, err
	}
	manifest = append(manifest, '\n')

	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return Manifest{}, err
	}
	outputs := []struct {
		name string
		data []byte
	}{
		{TrainFile, train}, {EvalFile, eval}, {ExclusionsFile, exclusions}, {ManifestFile, manifest},
	}
	for _, o := range outputs {
		path := filepath.Join(outDir, o.name)
		if err := os.WriteFile(path, o.data, 0o600); err != nil {
			return Manifest{}, fmt.Errorf("distill: writing %s: %w", path, err)
		}
	}
	return m, nil
}

func digest(path string, data []byte, lines int) FileDigest {
	sum := sha256.Sum256(data)
	return FileDigest{Path: path, SHA256: hex.EncodeToString(sum[:]), Lines: lines}
}

func countKind(exs []Example, kind string) int {
	n := 0
	for _, ex := range exs {
		if ex.Kind == kind {
			n++
		}
	}
	return n
}

// templatesOf lists each distinct template ID in first-seen order.
func templatesOf(exs []Example) []string {
	seen := map[string]bool{}
	var out []string
	for _, ex := range exs {
		if !seen[ex.Template] {
			seen[ex.Template] = true
			out = append(out, ex.Template)
		}
	}
	return out
}

// nonNilCounts keeps the reason map present (as {}) on an empty run, so a
// reader never has to special-case a missing key.
func nonNilCounts(c map[string]int) map[string]int {
	if c == nil {
		return map[string]int{}
	}
	return c
}

// ReadManifest loads a manifest written by Run, for diffing two runs.
func ReadManifest(path string) (Manifest, error) {
	var m Manifest
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("distill: %s is not a manifest: %w", path, err)
	}
	if m.Format != ManifestFormat || m.Version != ManifestVer {
		return m, fmt.Errorf("distill: %s is format %q version %d, want %q %d",
			path, m.Format, m.Version, ManifestFormat, ManifestVer)
	}
	return m, nil
}

// FactDiff reports the facts that entered and left the selection between two
// manifests. A non-empty result means a baked fact may be stale: the model
// trained on the old set still believes the removed ones.
func FactDiff(old, cur Manifest) (added, removed []FactDigest) {
	key := func(f FactDigest) string { return f.Note + "\x00" + f.ID + "\x00" + f.SHA256 }
	oldSet := map[string]bool{}
	for _, f := range old.Facts {
		oldSet[key(f)] = true
	}
	curSet := map[string]bool{}
	for _, f := range cur.Facts {
		curSet[key(f)] = true
		if !oldSet[key(f)] {
			added = append(added, f)
		}
	}
	for _, f := range old.Facts {
		if !curSet[key(f)] {
			removed = append(removed, f)
		}
	}
	return added, removed
}
