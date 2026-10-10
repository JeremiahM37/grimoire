package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// `grimoire memory export` and `grimoire memory import`: the whole memory store
// in and out of a file. The format is docs/PORTABILITY.md. Both go through the
// same HTTP handlers the server serves, in process, so the access rules are the
// ones the API applies, not a second copy of them.

const memoryPortabilityUsage = `  grimoire memory export [--out FILE] [--format jsonl|markdown] [--agent A] [--session S] [--category C]
  grimoire memory import FILE [--from auto|grimoire|mem0|letta|zep|jsonl-generic] [--dry-run] [--yes]`

func memoryExportCmd(e *env, args []string) int {
	format := strings.ToLower(flagOr(args, "--format", "jsonl"))
	if format != "jsonl" && format != "markdown" {
		return fail("--format must be jsonl or markdown")
	}
	q := url.Values{"format": {format}}
	for flag, param := range map[string]string{"--agent": "agent", "--session": "session", "--category": "category"} {
		if v, ok := flagValue(args, flag); ok {
			q.Set(param, v)
		}
	}
	status, raw := e.call("GET", "/api/memory/export?"+q.Encode())
	if status != http.StatusOK {
		return fail("export failed: %s", raw)
	}
	out, hasOut := flagValue(args, "--out")
	if !hasOut || out == "" {
		fmt.Print(raw)
		return 0
	}
	if err := os.WriteFile(out, []byte(raw), 0o600); err != nil {
		return fail("writing %s: %v", out, err)
	}
	fmt.Fprintf(os.Stderr, "exported %s to %s\n", exportSummary(raw, format), out)
	return 0
}

// exportSummary says how many facts an export holds, from its own header.
func exportSummary(raw, format string) string {
	if format == "markdown" {
		return "memory (markdown)"
	}
	first, _, _ := strings.Cut(raw, "\n")
	var h struct {
		Count int `json:"count"`
	}
	if json.Unmarshal([]byte(first), &h) == nil {
		return fmt.Sprintf("%d facts", h.Count)
	}
	return "memory"
}

func memoryImportCmd(e *env, args []string) int {
	pos := positional(args)
	if len(pos) == 0 {
		return fail("usage: grimoire memory import FILE [--from auto|grimoire|mem0|letta|zep|jsonl-generic] [--dry-run]")
	}
	dry := hasFlag(args, "--dry-run")
	if !dry && !requireExplicitVault("memory import", args) {
		return 2
	}
	src := pos[0]
	if strings.HasPrefix(src, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			src = home + src[1:]
		}
	}
	raw, err := os.ReadFile(src)
	if err != nil {
		return fail("cannot read %s: %v", src, err)
	}
	q := url.Values{"from": {flagOr(args, "--from", "auto")}}
	if dry {
		q.Set("dry_run", "1")
	}
	status, body := e.callRaw("POST", "/api/memory/import?"+q.Encode(), "application/octet-stream", raw)
	if status != http.StatusOK {
		return fail("import failed: %s", body)
	}
	var rep struct {
		Format     string `json:"format"`
		DryRun     bool   `json:"dry_run"`
		Total      int    `json:"total"`
		New        int    `json:"new"`
		Written    int    `json:"written"`
		Duplicates int    `json:"duplicates"`
		Failed     int    `json:"failed"`
		Skipped    []struct {
			Index  int    `json:"index"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Sample []string `json:"sample"`
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		return fail("%v", err)
	}
	if rep.DryRun {
		fmt.Printf("dry run (%s): %d records, %d would be written, %d already on file, %d skipped\n",
			rep.Format, rep.Total, rep.New, rep.Duplicates, len(rep.Skipped))
		for _, s := range rep.Sample {
			fmt.Printf("  + %s\n", truncTitle(s))
		}
		return 0
	}
	fmt.Printf("imported %d of %d (%s): %d already on file, %d skipped, %d failed\n",
		rep.Written, rep.Total, rep.Format, rep.Duplicates, len(rep.Skipped), rep.Failed)
	for _, s := range rep.Skipped {
		fmt.Fprintf(os.Stderr, "  skipped #%d: %s\n", s.Index, s.Reason)
	}
	return 0
}

// callRaw is callBody for a request whose body is the file itself rather than
// JSON. It runs through the same in-process handler.
func (e *env) callRaw(method, path, contentType string, body []byte) (int, string) {
	req, err := http.NewRequest(method, path, bytes.NewReader(body))
	if err != nil {
		return 500, err.Error()
	}
	req.Header.Set("Content-Type", contentType)
	rec := &recorder{status: 200}
	e.handler.ServeHTTP(rec, req)
	return rec.status, rec.body.String()
}
