package rerank

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------- remote

func TestRemoteCommonShape(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" {
			http.NotFound(w, r)
			return
		}
		auth = r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&got)
		// results come back sorted by relevance, not input order
		io.WriteString(w, `{"results":[{"index":2,"relevance_score":0.9},{"index":0,"relevance_score":0.5},{"index":1,"relevance_score":0.1}]}`)
	}))
	defer srv.Close()
	r, err := NewRemote(RemoteConfig{URL: srv.URL + "/v1/", APIKey: "k", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	s, err := r.Score(context.Background(), "q", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(s) != "[0.5 0.1 0.9]" {
		t.Fatalf("scores %v", s)
	}
	if auth != "Bearer k" || got["model"] != "m" || got["query"] != "q" || got["top_n"] != float64(3) {
		t.Fatalf("request %v auth %q", got, auth)
	}
	if docs, _ := got["documents"].([]any); len(docs) != 3 {
		t.Fatalf("documents %v", got["documents"])
	}
}

func TestRemoteFallsBackToTextsField(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["texts"]; !ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			io.WriteString(w, `{"error":"missing field texts"}`)
			return
		}
		io.WriteString(w, `[{"index":1,"score":2.5},{"index":0,"score":-1}]`)
	}))
	defer srv.Close()
	r, _ := NewRemote(RemoteConfig{URL: srv.URL + "/rerank"})
	for i := 0; i < 2; i++ {
		s, err := r.Score(context.Background(), "q", []string{"a", "b"})
		if err != nil || s[0] != -1 || s[1] != 2.5 {
			t.Fatalf("scores %v err %v", s, err)
		}
	}
	if calls != 3 { // one rejected attempt, then texts remembered
		t.Fatalf("%d calls", calls)
	}
}

func TestRemoteErrors(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"status": func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", http.StatusUnauthorized) },
		"index": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"results":[{"index":5,"relevance_score":1}]}`)
		},
		"shape": func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"ok":true}`) },
		"slow": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(2 * time.Second):
			case <-r.Context().Done():
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			r, _ := NewRemote(RemoteConfig{URL: srv.URL, Timeout: 200 * time.Millisecond})
			if _, err := r.Score(context.Background(), "q", []string{"a"}); err == nil {
				t.Fatal("no error")
			}
		})
	}
	if _, err := NewRemote(RemoteConfig{URL: "ftp://x"}); err == nil {
		t.Fatal("non-http URL accepted")
	}
}

func TestRemoteMissingResultsScoreLowest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"results":[{"index":1,"relevance_score":0.2}]}`)
	}))
	defer srv.Close()
	r, _ := NewRemote(RemoteConfig{URL: srv.URL})
	s, err := r.Score(context.Background(), "q", []string{"a", "b"})
	if err != nil || !math.IsInf(float64(s[0]), -1) || s[1] != 0.2 {
		t.Fatalf("%v %v", s, err)
	}
}
