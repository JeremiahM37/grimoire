package main

// The knowledge console is deliberately a terminal-native client of the
// canonical HTTP contract. It is useful over a pipe, but when attached to a
// terminal it stays open for successive questions and keeps the evidence close
// to the answer instead of reducing the feature to a renamed `ask` command.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/JeremiahM37/grimoire/go/internal/watcher"
)

func knowledgeJSON(args []string) bool { return hasFlag(args, "--json") }

func knowledgeValue(args []string, name string) (string, bool) {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, name+"=") {
			return strings.TrimPrefix(a, name+"="), true
		}
	}
	return "", false
}

func knowledgeArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--path" || a == "--depth" || a == "--limit" || a == "--after" || a == "--before" ||
			a == "--min-degree" || a == "--drop-noisy" || a == "--include-documents" ||
			a == "--include-chunks" || a == "--watch" {
			i++
			continue
		}
		if !(a == "--json" || a == "--plain" || a == "--expand" ||
			strings.HasPrefix(a, "--path=") || strings.HasPrefix(a, "--depth=") || strings.HasPrefix(a, "--limit=") ||
			strings.HasPrefix(a, "--after=") || strings.HasPrefix(a, "--before=") ||
			strings.HasPrefix(a, "--min-degree=") || strings.HasPrefix(a, "--drop-noisy=") ||
			strings.HasPrefix(a, "--include-documents=") || strings.HasPrefix(a, "--include-chunks=") ||
			strings.HasPrefix(a, "--watch=")) {
			out = append(out, a)
		}
	}
	return out
}

func knowledgeRequest(args []string, question string) map[string]any {
	body := map[string]any{"question": question}
	for _, spec := range []struct{ flag, key string }{{"--depth", "depth"}, {"--limit", "limit"}, {"--min-degree", "min_degree"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			if n, err := strconv.Atoi(v); err == nil {
				body[spec.key] = n
			}
		}
	}
	for _, spec := range []struct{ flag, key string }{{"--after", "after"}, {"--before", "before"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			body[spec.key] = v
		}
	}
	for _, spec := range []struct{ flag, key string }{{"--drop-noisy", "drop_noisy"}, {"--include-documents", "include_documents"}, {"--include-chunks", "include_chunks"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			body[spec.key] = strings.EqualFold(v, "true") || v == "1" || strings.EqualFold(v, "yes")
		} else if hasFlag(args, spec.flag) {
			body[spec.key] = true
		}
	}
	if hasFlag(args, "--expand") {
		body["expand"] = true
	}
	return body
}

func cmdKnowledge(args []string) int {
	if len(args) == 0 || args[0] != "extract" {
		return fail("usage: grimoire knowledge extract PATH... [--force] [--json]")
	}
	paths := make([]string, 0, len(args)-1)
	force := false
	jsonMode := false
	for _, arg := range args[1:] {
		switch arg {
		case "--force":
			force = true
		case "--json":
			jsonMode = true
		default:
			paths = append(paths, arg)
		}
	}
	if len(paths) == 0 || len(paths) > 10 {
		return fail("knowledge extract requires 1-10 note paths")
	}
	environment, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer environment.close()
	if _, err := environment.index.Sync(); err != nil {
		return fail("knowledge startup sync: %v", err)
	}
	status, body := environment.callBody("POST", "/api/knowledge/extract", map[string]any{
		"paths": paths,
		"force": force,
	})
	if status >= 400 {
		return fail("relationship extraction failed: %s", body)
	}
	var out struct {
		Results []struct {
			Path, Status string
			Triples      int    `json:"triples"`
			Error        string `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		fmt.Println(body)
		return 0
	}
	if jsonMode {
		fmt.Println(body)
		if extractionResultsFailed(out.Results) {
			return 1
		}
		return 0
	}
	for _, result := range out.Results {
		if result.Error != "" {
			fmt.Printf("%s\t%s\t%d\t%s\n", result.Path, result.Status, result.Triples, result.Error)
		} else {
			fmt.Printf("%s\t%s\t%d\n", result.Path, result.Status, result.Triples)
		}
	}
	if extractionResultsFailed(out.Results) {
		return 1
	}
	return 0
}

func extractionResultsFailed(results []struct {
	Path, Status string
	Triples      int    `json:"triples"`
	Error        string `json:"error"`
}) bool {
	for _, result := range results {
		if result.Error != "" || strings.EqualFold(result.Status, "error") {
			return true
		}
	}
	return false
}

func cmdKnowledgeQuery(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if _, err := e.index.Sync(); err != nil {
		return fail("knowledge startup sync: %v", err)
	}

	jsonMode := knowledgeJSON(args)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var watchDone chan struct{}
	var watchErr chan error
	if folder, ok := knowledgeValue(args, "--watch"); ok && folder != "" {
		store := e.server.Documents
		if store == nil {
			return fail("document watch unavailable: server document store is not configured")
		}
		if err := store.InitialReconcile(ctx, folder); err != nil {
			return fail("document watch initial reconcile: %v", err)
		}
		watchDone = make(chan struct{})
		watchErr = make(chan error, 1)
		go func() {
			defer close(watchDone)
			if err := store.Watch(ctx, folder); err != nil && err != context.Canceled {
				watchErr <- err
				fmt.Fprintf(os.Stderr, "document watch: %v\n", err)
			}
		}()
	}
	questions := knowledgeArgs(args)
	var result int
	if len(questions) > 0 {
		result = runKnowledgeQuestion(e, args, strings.Join(questions, " "), jsonMode)
	} else {
		var vaultWatcher *watcher.Watcher
		if _, hasExternalWatch := knowledgeValue(args, "--watch"); !hasExternalWatch && isTerminal(os.Stdin) {
			vaultWatcher = watcher.New(e.vault, e.index, 0)
			if err := vaultWatcher.Start(); err != nil {
				return fail("vault watch: %v", err)
			}
		}
		result = runKnowledgeConsole(e, args, os.Stdin, jsonMode, isTerminal(os.Stdin))
		if vaultWatcher != nil {
			vaultWatcher.Stop()
		}
	}
	cancel()
	if watchDone != nil {
		<-watchDone
	}
	if watchErr != nil {
		select {
		case err := <-watchErr:
			if result == 0 {
				result = fail("document watch: %v", err)
			}
		default:
		}
	}
	return result
}

func runKnowledgeConsole(e *env, args []string, in io.Reader, jsonMode, interactive bool) int {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	if interactive {
		fmt.Fprintln(os.Stderr, "Grimoire knowledge console — ask a question, or Ctrl-D/Ctrl-C to exit.")
	}
	lines := make(chan string)
	scanDone := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(in)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		scanDone <- scanner.Err()
	}()
	for {
		if interactive {
			fmt.Fprint(os.Stderr, "> ")
		}
		select {
		case <-stop:
			return 130
		case question := <-lines:
			question = strings.TrimSpace(question)
			if question == "" {
				continue
			}
			if question == ":quit" || question == ":q" {
				return 0
			}
			if strings.HasPrefix(question, ":source ") {
				_ = runKnowledgeSourceInteractive(e, strings.TrimSpace(strings.TrimPrefix(question, ":source ")), jsonMode)
				continue
			}
			if strings.HasPrefix(question, ":graph") {
				_ = runKnowledgeGraphInteractive(e, strings.TrimSpace(strings.TrimPrefix(question, ":graph")), jsonMode)
				continue
			}
			if code := runKnowledgeQuestion(e, args, question, jsonMode); code != 0 {
				fmt.Fprintln(os.Stderr, "query failed; console remains available")
			}
		case err := <-scanDone:
			if err != nil {
				return fail("query input: %v", err)
			}
			return 0
		}
	}
}

func isTerminal(r *os.File) bool {
	st, err := r.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func runKnowledgeQuestion(e *env, args []string, question string, jsonMode bool) int {
	status, body := e.callBody("POST", "/api/knowledge/query", knowledgeRequest(args, question))
	if status >= 400 {
		return fail("knowledge query failed: %s", body)
	}
	if jsonMode || hasFlag(args, "--plain") {
		if jsonMode {
			fmt.Println(body)
			return 0
		}
		var v map[string]any
		if json.Unmarshal([]byte(body), &v) == nil {
			printKnowledgeText(v)
		} else {
			fmt.Println(body)
		}
		return 0
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		return fail("invalid knowledge response: %v", err)
	}
	printKnowledgeText(v)
	return 0
}

func runKnowledgeSourceInteractive(e *env, path string, jsonMode bool) int {
	status, body := e.call("GET", "/api/knowledge/source?path="+url.QueryEscape(path))
	if status >= 400 {
		return fail("knowledge source failed: %s", body)
	}
	if jsonMode {
		fmt.Println(body)
		return 0
	}
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) == nil {
		fmt.Printf("%s\n%s\n\n%s\n", v["title"], v["path"], v["text"])
	} else {
		fmt.Println(body)
	}
	return 0
}

func runKnowledgeGraphInteractive(e *env, seed string, jsonMode bool) int {
	q := url.Values{}
	if seed != "" {
		q.Set("seed", seed)
	}
	status, body := e.call("GET", "/api/knowledge/graph?"+q.Encode())
	if status >= 400 {
		return fail("knowledge graph failed: %s", body)
	}
	if jsonMode {
		fmt.Println(body)
		return 0
	}
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) == nil {
		printKnowledgeText(map[string]any{"graph": v})
	} else {
		fmt.Println(body)
	}
	return 0
}

func printKnowledgeText(v map[string]any) {
	if answer, ok := v["answer"].(string); ok && answer != "" {
		fmt.Println(answer)
	}
	if citations, ok := v["citations"].([]any); ok && len(citations) > 0 {
		fmt.Println("\nSources:")
		for i, raw := range citations {
			c, _ := raw.(map[string]any)
			fmt.Printf("  [%d] %s — %s\n", i+1, c["title"], c["path"])
			if text, ok := c["text"].(string); ok && strings.TrimSpace(text) != "" {
				fmt.Printf("      %s\n", strings.TrimSpace(text))
			}
		}
	}
	if graph, ok := v["graph"].(map[string]any); ok {
		if nodes, ok := graph["nodes"].([]any); ok {
			for _, raw := range nodes {
				n, _ := raw.(map[string]any)
				fmt.Printf("  node %-12v %v (%v)\n", n["id"], n["label"], n["kind"])
			}
		}
		if edges, ok := graph["edges"].([]any); ok {
			for _, raw := range edges {
				e, _ := raw.(map[string]any)
				fmt.Printf("  edge %v -[%v]-> %v\n", e["source"], e["relation"], e["target"])
				if ev, ok := e["evidence"].([]any); ok {
					for _, eraw := range ev {
						evidence, _ := eraw.(map[string]any)
						fmt.Printf("      evidence: %v (%v)\n", evidence["title"], evidence["path"])
					}
				}
			}
		}
		if nodes, ok := graph["nodes"].([]any); ok {
			fmt.Printf("\nGraph: %d nodes", len(nodes))
		}
		if edges, ok := graph["edges"].([]any); ok {
			fmt.Printf(", %d relationships", len(edges))
		}
		fmt.Println()
	}
}

func cmdKnowledgeGraph(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if _, err := e.index.Sync(); err != nil {
		return fail("knowledge startup sync: %v", err)
	}
	q := url.Values{}
	for _, spec := range []struct{ flag, key string }{{"--seed", "seed"}, {"--relation", "relation"}, {"--q", "q"}, {"--min-degree", "min_degree"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			q.Set(spec.key, v)
		}
	}
	for _, spec := range []struct{ flag, key string }{{"--depth", "depth"}, {"--limit", "limit"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			q.Set(spec.key, v)
		}
	}
	for _, spec := range []struct{ flag, key string }{{"--drop-noisy", "drop_noisy"}, {"--include-documents", "include_documents"}, {"--include-chunks", "include_chunks"}} {
		if v, ok := knowledgeValue(args, spec.flag); ok {
			q.Set(spec.key, v)
		} else if hasFlag(args, spec.flag) {
			q.Set(spec.key, "true")
		}
	}
	status, body := e.call("GET", "/api/knowledge/graph?"+q.Encode())
	if status >= 400 {
		return fail("knowledge graph failed: %s", body)
	}
	if knowledgeJSON(args) {
		fmt.Println(body)
		return 0
	}
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) != nil {
		fmt.Println(body)
		return 0
	}
	printKnowledgeText(map[string]any{"graph": v})
	return 0
}

func cmdKnowledgeSource(args []string) int {
	paths := knowledgeArgs(args)
	if len(paths) != 1 {
		return fail("usage: grimoire source PATH [--json]")
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if _, err := e.index.Sync(); err != nil {
		return fail("knowledge startup sync: %v", err)
	}
	status, body := e.call("GET", "/api/knowledge/source?path="+url.QueryEscape(paths[0]))
	if status >= 400 {
		return fail("knowledge source failed: %s", body)
	}
	if knowledgeJSON(args) {
		fmt.Println(body)
		return 0
	}
	var v map[string]any
	if json.Unmarshal([]byte(body), &v) == nil {
		fmt.Printf("%s\n%s\n\n%s\n", v["title"], v["path"], v["text"])
	} else {
		fmt.Println(body)
	}
	return 0
}

func cmdDocuments(args []string) int {
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	if len(args) > 0 && args[0] == "watch" {
		if len(args) != 2 {
			return fail("usage: grimoire documents watch FOLDER")
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fmt.Fprintf(os.Stderr, "watching documents in %s (Ctrl-C to stop)\n", args[1])
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
		defer signal.Stop(stop)
		store := e.server.Documents
		if store == nil {
			return fail("document watch unavailable: server document store is not configured")
		}
		go func() { <-stop; cancel() }()
		if err := store.Watch(ctx, args[1]); err != nil && err != context.Canceled {
			return fail("document watch: %v", err)
		}
		return 0
	}
	if len(args) > 0 && args[0] == "refresh" {
		if len(args) != 2 {
			return fail("usage: grimoire documents refresh PATH")
		}
		status, body := e.callBody("POST", "/api/documents/refresh", map[string]any{"path": args[1]})
		if status >= 400 {
			return fail("document refresh failed: %s", body)
		}
		if knowledgeJSON(args) {
			fmt.Println(body)
		} else {
			fmt.Printf("refreshed %s\n", args[1])
		}
		return 0
	}
	status, body := e.call("GET", "/api/documents")
	if status >= 400 {
		return fail("documents failed: %s", body)
	}
	if knowledgeJSON(args) {
		fmt.Println(body)
		return 0
	}
	var v struct {
		Documents []struct{ Path, SourcePath, Title, Format, Status string } `json:"documents"`
	}
	if json.Unmarshal([]byte(body), &v) != nil {
		fmt.Println(body)
		return 0
	}
	for _, d := range v.Documents {
		fmt.Printf("%-40s %-12s %s\n", d.Path, d.Status, d.Title)
	}
	return 0
}

func cmdDocumentImport(args []string) int {
	paths := knowledgeArgs(args)
	if len(paths) != 1 {
		return fail("usage: grimoire document-import FILE [--path EXISTING_NOTE] [--json]")
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		return fail("%v", err)
	}
	e, err := openEnv()
	if err != nil {
		return fail("%v", err)
	}
	defer e.close()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if target, ok := knowledgeValue(args, "--path"); ok {
		if err := mw.WriteField("path", target); err != nil {
			return fail("%v", err)
		}
	}
	part, err := mw.CreateFormFile("file", filepath.Base(paths[0]))
	if err != nil {
		return fail("%v", err)
	}
	if _, err = part.Write(raw); err != nil {
		return fail("%v", err)
	}
	mw.Close()
	req := httptestRequest("POST", "/api/documents/import", &buf, mw.FormDataContentType())
	rec := &recorder{status: 200}
	e.handler.ServeHTTP(rec, req)
	if rec.status >= 400 {
		return fail("document import failed: %s", rec.body.String())
	}
	if knowledgeJSON(args) {
		fmt.Println(rec.body.String())
	} else {
		fmt.Printf("imported %s\n", paths[0])
	}
	return 0
}

func httptestRequest(method, path string, body io.Reader, contentType string) *http.Request {
	r, _ := http.NewRequest(method, path, body)
	r.Header.Set("Content-Type", contentType)
	return r
}
