package secscan

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/JeremiahM37/grimoire/go/internal/dream"
	"github.com/JeremiahM37/grimoire/go/internal/trust/normalize"
)

// TestInjectionCorpusMeasure is the scanner half of benchmarks/injection/
// measure_inject.py. It is skipped unless the harness sets the two env vars:
// the harness generates the payloads and the benign set, this test only scores
// them with the same injection check the vault runs, once on the raw text
// (the pre-normalizer behaviour) and once through normalize.Expand.
//
//	INJECTION_BENCH_IN   JSON input: {"attacks": [...], "benign": [...]}
//	INJECTION_BENCH_OUT  where the JSON result is written
func TestInjectionCorpusMeasure(t *testing.T) {
	in, out := os.Getenv("INJECTION_BENCH_IN"), os.Getenv("INJECTION_BENCH_OUT")
	if in == "" || out == "" {
		t.Skip("set INJECTION_BENCH_IN and INJECTION_BENCH_OUT (run benchmarks/injection/measure_inject.py)")
	}
	raw, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var req struct {
		Attacks []struct {
			ID   string `json:"id"`
			Text string `json:"text"`
		} `json:"attacks"`
		Benign []struct {
			Path string `json:"path"`
			Body string `json:"body"`
		} `json:"benign"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatal(err)
	}

	type attackRes struct {
		ID         string `json:"id"`
		Raw        bool   `json:"raw"`
		Normalized bool   `json:"normalized"`
		// Legacy is the normalized reading judged by the original pattern
		// list only, so the gain from the phrase families can be isolated.
		Legacy    bool     `json:"legacy"`
		ExposedBy []string `json:"exposed_by,omitempty"`
	}
	res := struct {
		Attacks            []attackRes `json:"attacks"`
		BenignDocs         int         `json:"benign_docs"`
		BenignLines        int         `json:"benign_lines"`
		DocsRaw            int         `json:"benign_docs_flagged_raw"`
		DocsNorm           int         `json:"benign_docs_flagged_normalized"`
		LinesRaw           int         `json:"benign_lines_flagged_raw"`
		LinesNorm          int         `json:"benign_lines_flagged_normalized"`
		BenignFlagged      []string    `json:"benign_flagged_normalized,omitempty"`
		BenignFlaggedLines []string    `json:"benign_flagged_lines,omitempty"`
	}{}

	rawReadings := func(line string) []normalize.Variant {
		return []normalize.Variant{{Text: line}}
	}

	for _, a := range req.Attacks {
		doc := dream.Doc{Path: "attack/" + a.ID, Body: a.Text, Kind: dream.KindFileMemory}
		ar := attackRes{ID: a.ID}
		ar.Raw = legacyRawFlagged(a.Text)
		norm := scanWith([]dream.Doc{doc}, nil, normalize.Expand)
		ar.Normalized = hasInjection(norm)
		ar.Legacy = legacyFlagged(a.Text)
		ar.ExposedBy = exposure(a.Text)
		res.Attacks = append(res.Attacks, ar)
	}

	for _, d := range req.Benign {
		doc := dream.Doc{Path: d.Path, Body: d.Body, Kind: dream.KindFileMemory}
		lines := strings.Split(d.Body, "\n")
		res.BenignDocs++
		for _, l := range lines {
			if strings.TrimSpace(l) != "" {
				res.BenignLines++
			}
		}
		rawFs := scanWith([]dream.Doc{doc}, nil, rawReadings)
		normFs := scanWith([]dream.Doc{doc}, nil, normalize.Expand)
		if legacyRawFlagged(d.Body) {
			res.DocsRaw++
		}
		for _, f := range normFs {
			if f.Check == checkInjection {
				res.BenignFlaggedLines = append(res.BenignFlaggedLines, fmt.Sprintf("%s:%d %s", d.Path, f.Line, f.Excerpt))
			}
		}
		if hasInjection(normFs) {
			res.DocsNorm++
			res.BenignFlagged = append(res.BenignFlagged, d.Path)
		}
		res.LinesRaw += legacyRawLines(d.Body)
		_ = rawFs
		res.LinesNorm += countInjection(normFs)
	}

	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasInjection(fs []dream.Finding) bool { return countInjection(fs) > 0 }

func countInjection(fs []dream.Finding) int {
	n := 0
	for _, f := range fs {
		if f.Check == checkInjection {
			n++
		}
	}
	return n
}

// legacyFlagged reports whether any reading of the text matches the original
// pattern list. It is the pre-family behaviour on normalized text.
// legacyRawFlagged is the pre-change check: the original list on the text as
// written, no normalization. It is the baseline the other numbers are against.
func legacyRawFlagged(text string) bool { return legacyRawLines(text) > 0 }

func legacyRawLines(text string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		for _, re := range legacyInjectionPatterns {
			if re.MatchString(line) {
				n++
				break
			}
		}
	}
	return n
}

func legacyFlagged(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		for _, v := range normalize.Expand(line) {
			for _, re := range legacyInjectionPatterns {
				if re.MatchString(v.Text) {
					return true
				}
			}
		}
	}
	return false
}

// exposure returns the transform chain of the first reading of the payload
// that the injection check flags, or nil when no reading is flagged. It mirrors
// the loop in scanDoc so the report can say which step made the difference.
func exposure(text string) []string {
	for _, line := range strings.Split(text, "\n") {
		for _, v := range normalize.Expand(line) {
			if injectionLine(v.Text) {
				if len(v.Transforms) == 0 {
					return []string{"raw"}
				}
				return v.Transforms
			}
		}
	}
	return nil
}
