package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/decide"
	"github.com/JeremiahM37/grimoire/go/internal/memory"
	"github.com/JeremiahM37/grimoire/go/internal/memstore"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
)

// `grimoire memory loaded-audit` asks, for each agent memory file that
// MEMORY.md links to, whether the file body adds anything beyond the one-line
// summary every session already loads. A file that only restates its summary
// is a candidate for the loaded experiment (docs/MEMORY_USE.md). It runs
// offline: nothing is written, and the vault is not opened.

// loadedQuestion and loadedOptions are the one question the audit asks. They
// are constants so every run asks exactly the same thing.
const loadedQuestion = "Does the file body add anything an agent needs beyond its one-line summary?"

var loadedOptions = map[string]string{
	"restates":    "the body says nothing the summary does not already say",
	"refines":     "the body adds detail, conditions or reasons the summary leaves out",
	"contradicts": "the body says something the summary gets wrong or conflicts with",
	"independent": "the body covers something the summary does not describe at all",
}

// loadedOrder is the order options are printed and counted in.
var loadedOrder = []string{"restates", "refines", "contradicts", "independent"}

// fallback bands on the share of body words the summary does not contain.
// They are a deterministic stand-in with no judgement in them, so
// "contradicts" cannot be measured by them at all.
const (
	fallbackRestatesBelow   = 0.20
	fallbackIndependentFrom = 0.50
)

// loadedBodyLimit bounds how much of a body is sent to the decision server.
const loadedBodyLimit = 4000

var (
	loadedLinkRE   = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)\)`)
	loadedBulletRE = regexp.MustCompile(`^[\s\-*>]+`)
)

// loadedEntry is one MEMORY.md line that links a memory file: the file, and
// the line with its links reduced to their labels, which is the summary.
type loadedEntry struct {
	File    string
	Summary string
}

// parseLoadedIndex returns the first line that links each memory file in the
// same directory, in index order. A file linked twice counts once.
func parseLoadedIndex(index string) []loadedEntry {
	var out []loadedEntry
	seen := map[string]bool{}
	for _, line := range strings.Split(index, "\n") {
		file := ""
		for _, m := range loadedLinkRE.FindAllStringSubmatch(line, -1) {
			t := m[2]
			if strings.Contains(t, "://") || strings.HasPrefix(t, "#") {
				continue
			}
			t = strings.TrimPrefix(t, "./")
			if !strings.HasSuffix(t, ".md") || strings.Contains(t, "/") {
				continue
			}
			file = t
			break
		}
		if file == "" || seen[file] {
			continue
		}
		seen[file] = true
		summary := loadedLinkRE.ReplaceAllString(line, "$1")
		summary = strings.TrimSpace(loadedBulletRE.ReplaceAllString(summary, ""))
		out = append(out, loadedEntry{File: file, Summary: summary})
	}
	return out
}

// absentShare is the share of a body's distinct content words that the summary
// does not contain. An empty body has nothing the summary lacks.
func absentShare(summary, body string) float64 {
	have := map[string]bool{}
	for _, t := range memory.Tokens(summary) {
		have[t] = true
	}
	want := map[string]bool{}
	for _, t := range memory.Tokens(body) {
		want[t] = true
	}
	if len(want) == 0 {
		return 0
	}
	absent := 0
	for t := range want {
		if !have[t] {
			absent++
		}
	}
	return float64(absent) / float64(len(want))
}

// fallbackOption maps an absent share onto the option names, so the output
// has the same shape with or without a decision server.
func fallbackOption(share float64) string {
	switch {
	case share < fallbackRestatesBelow:
		return "restates"
	case share < fallbackIndependentFrom:
		return "refines"
	default:
		return "independent"
	}
}

type loadedReport struct {
	Mode        string              `json:"mode"`
	Model       string              `json:"model,omitempty"`
	Directory   string              `json:"directory"`
	Files       int                 `json:"files"`
	Missing     int                 `json:"missing"`
	Empty       int                 `json:"empty"`
	Counts      map[string]int      `json:"counts"`
	Paths       map[string][]string `json:"paths"`
	MeanAbsent  *float64            `json:"mean_absent_share,omitempty"`
	Contradicts *string             `json:"contradicts,omitempty"`
}

func memoryLoadedAuditCmd(args []string) int {
	gdir := filepath.Join(envOr("GRIMOIRE_VAULT", filepath.Join(os.Getenv("HOME"), "notes")), ".grimoire")
	st := settings.New(gdir)

	dir := flagOr(args, "--memory-dir", "")
	if dir == "" {
		dir = st.Get("memory_canonical_dir")
	}
	if dir == "" {
		dir = st.Get("dream_canonical_memory")
	}
	if dir == "" {
		dir = filepath.Join(os.Getenv("HOME"), ".grimoire", "memory")
	}
	dir = expandPath(dir)

	limit, err := nonNegativeFlag(args, "--limit", 0)
	if err != nil {
		return fail("%v", err)
	}
	url := strings.TrimSpace(flagOr(args, "--decision-url", st.Get("decision_url")))
	asJSON := hasFlag(args, "--json")

	raw, err := os.ReadFile(filepath.Join(dir, memstore.IndexName))
	if err != nil {
		return fail("loaded-audit: no %s in %s: %v", memstore.IndexName, dir, err)
	}
	entries := parseLoadedIndex(string(raw))
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}

	rep := loadedReport{Directory: dir, Counts: map[string]int{}, Paths: map[string][]string{}}
	for _, o := range loadedOrder {
		rep.Counts[o] = 0
		rep.Paths[o] = []string{}
	}
	var client *decide.Client
	if url != "" {
		key := strings.TrimSpace(st.Get("decision_api_key"))
		if key == "" {
			if v, err := secrets.New(gdir).Get("decision-api-key"); err == nil {
				key = v
			}
		}
		rep.Mode, rep.Model = "decision", st.Get("decision_model")
		client = &decide.Client{BaseURL: url, Model: rep.Model, APIKey: key,
			HTTP: &http.Client{Timeout: 30 * time.Second}}
	} else {
		rep.Mode = "fallback"
	}

	var shareSum float64
	for _, en := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, en.File))
		if err != nil {
			rep.Missing++
			continue
		}
		note := memstore.ParseNote(en.File, string(raw), time.Time{})
		rep.Files++
		body := note.Body
		if body == "" {
			// Nothing beyond the summary to judge, so no call is made and the
			// file restates its summary by definition.
			rep.Empty++
		}

		if rep.Mode == "fallback" {
			share := absentShare(en.Summary, body)
			shareSum += share
			opt := fallbackOption(share)
			rep.Counts[opt]++
			rep.Paths[opt] = append(rep.Paths[opt], en.File)
			continue
		}
		if body == "" {
			rep.Counts["restates"]++
			rep.Paths["restates"] = append(rep.Paths["restates"], en.File)
			continue
		}
		opt, err := askLoaded(client, en.Summary, body)
		if err != nil {
			return fail("loaded-audit: decision server failed on %s after %d files: %v",
				en.File, rep.Files-1, err)
		}
		rep.Counts[opt]++
		rep.Paths[opt] = append(rep.Paths[opt], en.File)
	}
	if rep.Mode == "fallback" && rep.Files > 0 {
		mean := shareSum / float64(rep.Files)
		rep.MeanAbsent = &mean
	}
	if rep.Mode == "fallback" {
		none := "not measured without a decision server"
		rep.Contradicts = &none
	}

	if asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Println(string(out))
		return 0
	}
	printLoadedReport(rep)
	return 0
}

// askLoaded asks the decision server the audit question about one file and
// returns the option it chose.
func askLoaded(client *decide.Client, summary, body string) (string, error) {
	if r := []rune(body); len(r) > loadedBodyLimit {
		body = string(r[:loadedBodyLimit]) + "…"
	}
	state := "Index line: " + summary + "\n\nFile body:\n" + body
	criteria := make(map[string]string, len(loadedOptions))
	for k, v := range loadedOptions {
		criteria[k] = v
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := client.Ask(ctx, state, map[string]decide.Question{
		"beyond": {Type: decide.Choice, Instructions: loadedQuestion, Criteria: criteria},
	})
	if err != nil {
		return "", err
	}
	label := res.Answers["beyond"].Choice
	if _, ok := loadedOptions[label]; !ok {
		return "", fmt.Errorf("answer %q is not one of the options", label)
	}
	return label, nil
}

func printLoadedReport(rep loadedReport) {
	fmt.Printf("memory directory %s\n", rep.Directory)
	fmt.Printf("linked from %s: %d files judged, %d missing, %d with no body\n",
		memstore.IndexName, rep.Files, rep.Missing, rep.Empty)
	if rep.Mode == "decision" {
		fmt.Printf("decision server (model %s)\n", rep.Model)
		fmt.Println(`question: "` + loadedQuestion + `"`)
	} else {
		fmt.Println("no decision server configured: deterministic fallback, not the decision question")
		fmt.Printf("bands on the share of body words the summary lacks: restates below %.2f, refines below %.2f, independent above\n",
			fallbackRestatesBelow, fallbackIndependentFrom)
		fmt.Printf("mean share of body words absent from the summary: %.2f\n", *rep.MeanAbsent)
		fmt.Println("contradicts: not measured without a decision server")
	}
	var parts []string
	for _, o := range loadedOrder {
		parts = append(parts, fmt.Sprintf("%s %d", o, rep.Counts[o]))
	}
	fmt.Println(strings.Join(parts, "  "))
	for _, o := range loadedOrder {
		if len(rep.Paths[o]) == 0 {
			continue
		}
		fmt.Printf("%s:\n", o)
		for _, p := range rep.Paths[o] {
			fmt.Printf("  %s\n", p)
		}
	}
}
