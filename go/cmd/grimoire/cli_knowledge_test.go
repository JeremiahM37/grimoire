package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/watcher"
)

type editBetweenLines struct {
	path      string
	first     bool
	allowEdit <-chan struct{}
}

func (reader *editBetweenLines) Read(buffer []byte) (int, error) {
	if reader.first {
		reader.first = false
		copy(buffer, "owner?\n")
		return len("owner?\n"), nil
	}
	<-reader.allowEdit
	if err := os.WriteFile(reader.path, []byte("# Ownership\n\nThe owner is Ada.\n"), 0o644); err != nil {
		return 0, err
	}
	time.Sleep(100 * time.Millisecond)
	copy(buffer, "owner?\n")
	return len("owner?\n"), io.EOF
}

func TestKnowledgeArgsStripBothFlagForms(t *testing.T) {
	got := knowledgeArgs([]string{"what", "--depth", "2", "--limit=4", "--after", "2026-01-01", "now"})
	if strings.Join(got, " ") != "what now" {
		t.Fatalf("args = %v", got)
	}
	body := knowledgeRequest([]string{"--depth=2", "--min-degree", "3", "--expand", "--drop-noisy", "true", "--include-chunks"}, "q")
	for key, want := range map[string]any{"depth": 2, "min_degree": 3, "expand": true, "drop_noisy": true, "include_chunks": true} {
		if body[key] != want {
			t.Errorf("%s = %v, want %v", key, body[key], want)
		}
	}
}

func TestKnowledgeConsoleReadsPipedLinesAndSurvivesErrors(t *testing.T) {
	var calls int
	e := &env{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "temporary", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"answer":"ok","citations":[]}`)
	})}
	if code := runKnowledgeConsole(e, nil, strings.NewReader("first\nsecond\n"), true, false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestKnowledgeConsoleSourceAndGraphNavigation(t *testing.T) {
	var paths []string
	e := &env{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/knowledge/source" {
			io.WriteString(w, `{"path":"a.md","title":"A","text":"evidence"}`)
			return
		}
		io.WriteString(w, `{"nodes":[{"id":"a","label":"A","kind":"entity"}],"edges":[{"source":"a","target":"b","relation":"LINKS","evidence":[{"path":"a.md","title":"A"}]}]}`)
	})}
	if code := runKnowledgeConsole(e, nil, strings.NewReader(":source a.md\n:graph ops\n"), false, false); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Join(paths, ",") != "/api/knowledge/source,/api/knowledge/graph" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestKnowledgeGraphUsesNavigationFilters(t *testing.T) {
	// Query construction is tested without starting a server; this guards the
	// public filter spelling used by the CLI and coordinator handler.
	if v, ok := knowledgeValue([]string{"--drop-noisy", "true"}, "--drop-noisy"); !ok || v != "true" {
		t.Fatal("drop-noisy parsing")
	}
}

func TestKnowledgeConsoleSeesEditedVaultNoteBetweenQuestions(t *testing.T) {
	vaultRoot := t.TempDir()
	setPath := filepath.Join(vaultRoot, "ownership.md")
	if err := os.WriteFile(setPath, []byte("# Ownership\n\nThe owner is Lin.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GRIMOIRE_VAULT", vaultRoot)
	environment, err := newEnv(false)
	if err != nil {
		t.Fatal(err)
	}
	defer environment.close()
	if _, err := environment.index.Reindex(); err != nil {
		t.Fatal(err)
	}
	vaultWatcher := watcher.New(environment.vault, environment.index, 20*time.Millisecond)
	if err := vaultWatcher.Start(); err != nil {
		t.Fatal(err)
	}
	defer vaultWatcher.Stop()
	allowEdit := make(chan struct{})
	originalHandler := environment.handler
	var requestCount int
	environment.handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/knowledge/query" {
			requestCount++
			if requestCount == 1 {
				close(allowEdit)
			}
		}
		originalHandler.ServeHTTP(writer, request)
	})
	output := captureKnowledgeOutput(t, func() int {
		return runKnowledgeConsole(environment, nil, &editBetweenLines{path: setPath, first: true, allowEdit: allowEdit}, true, false)
	})
	if !strings.Contains(output, "Lin") || !strings.Contains(output, "Ada") {
		t.Fatalf("console did not observe both file versions: %q", output)
	}
}

func captureKnowledgeOutput(t *testing.T, run func() int) string {
	t.Helper()
	old := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	run()
	writer.Close()
	os.Stdout = old
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
