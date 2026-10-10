package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

// The terminal half of the realtime memory feed and the profile.
//
// `memory watch` runs the stream handler in process, the same way every other
// memory command does, and prints each event as it arrives. It needs no server,
// and it ends on Ctrl-C, which cancels the request and lets the handler return.

// streamWriter is an http.ResponseWriter that hands the body to a pipe as it is
// written, rather than buffering it the way the one-shot command recorder does.
// A non-200 status is an error response, which is kept whole for the caller.
type streamWriter struct {
	pw      *io.PipeWriter
	header  http.Header
	status  int
	errBody bytes.Buffer
}

func (w *streamWriter) Header() http.Header { return w.header }

func (w *streamWriter) WriteHeader(code int) { w.status = code }

func (w *streamWriter) Write(b []byte) (int, error) {
	if w.status != http.StatusOK {
		return w.errBody.Write(b)
	}
	return w.pw.Write(b)
}

// Flush has nothing to do: the pipe hands each write to the reader at once.
func (w *streamWriter) Flush() {}

func memoryWatchCmd(e *env, args []string) int {
	q := url.Values{}
	if v, ok := flagValue(args, "--agent"); ok {
		q.Set("agent", v)
	}
	if v, ok := flagValue(args, "--session"); ok {
		q.Set("session", v)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "/api/memory/stream?"+q.Encode(), nil)
	if err != nil {
		return fail("%v", err)
	}

	pr, pw := io.Pipe()
	sw := &streamWriter{pw: pw, header: http.Header{}, status: http.StatusOK}
	done := make(chan struct{})
	go func() {
		e.handler.ServeHTTP(sw, req)
		pw.Close()
		close(done)
	}()

	var event, data string
	scanner := bufio.NewScanner(pr)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data = strings.TrimPrefix(line, "data: ")
		case line == "" && data != "":
			fmt.Println(formatWatchLine(event, data))
			event, data = "", ""
		}
	}
	<-done
	if sw.status != http.StatusOK {
		return fail("watch failed (%d): %s", sw.status, strings.TrimSpace(sw.errBody.String()))
	}
	return 0
}

// formatWatchLine is one event as a line a person can scan: when, what, which
// fact, who wrote it, and the text (with the text it replaced, when it did).
func formatWatchLine(event, data string) string {
	var ev struct {
		At, ID, Path, Agent, Text, Reason string
		ReplacedText                      string `json:"replaced_text"`
		ContestedID                       string `json:"contested_id"`
		ChallengerID                      string `json:"challenger_id"`
	}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		return fmt.Sprintf("%s  %s", event, data)
	}
	detail := ev.Text
	if ev.ReplacedText != "" {
		detail = fmt.Sprintf("%s  (was: %s)", ev.Text, ev.ReplacedText)
	}
	if ev.Reason != "" {
		detail = fmt.Sprintf("[%s] %s", ev.Reason, detail)
	}
	if ev.ChallengerID != "" {
		detail += "  (challenged by " + ev.ChallengerID + ")"
	}
	if ev.ContestedID != "" {
		detail += "  (contests " + ev.ContestedID + ")"
	}
	who := ev.Agent
	if ev.Path != "" {
		who += " " + ev.Path
	}
	return fmt.Sprintf("%s  %-18s %s  %s  %s", ev.At, event, ev.ID, strings.TrimSpace(who), detail)
}

// memoryProfileCmd prints the profile. The markdown is what an agent reads; the
// footer says when a model rewrite was asked for and not used, and why.
func memoryProfileCmd(e *env, args []string) int {
	q := url.Values{}
	if v, ok := flagValue(args, "--subject"); ok {
		q.Set("subject", v)
	}
	if v, ok := flagValue(args, "--agent"); ok {
		q.Set("agent", v)
	}
	if v, ok := flagValue(args, "--budget"); ok {
		q.Set("budget", v)
	}
	if hasFlag(args, "--synthesize") {
		q.Set("synthesize", "true")
	}
	status, raw := e.call("GET", "/api/memory/profile?"+q.Encode())
	if status != http.StatusOK {
		return fail("profile failed: %s", raw)
	}
	if hasFlag(args, "--json") {
		fmt.Println(raw)
		return 0
	}
	var p struct {
		Markdown      string `json:"markdown"`
		Tokens        int    `json:"tokens"`
		Budget        int    `json:"budget"`
		Synthesized   bool   `json:"synthesized"`
		Fallback      bool   `json:"fallback"`
		Reason        string `json:"reason"`
		Deterministic string `json:"deterministic"`
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return fail("%v", err)
	}
	fmt.Print(p.Markdown)
	if p.Fallback {
		fmt.Fprintf(os.Stderr, "note: model rewrite not used (%s); showing the verifiable selection\n", p.Reason)
	}
	fmt.Fprintf(os.Stderr, "%d of %d tokens\n", p.Tokens, p.Budget)
	return 0
}
