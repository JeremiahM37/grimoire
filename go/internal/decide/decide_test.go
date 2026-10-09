package decide

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func server(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, APIKey: "k", HTTP: &http.Client{Timeout: time.Second}}
}

func TestAskSpeaksTheJevWireFormat(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("request %s auth %q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		q := body["questions"].(map[string]any)["changes"].(map[string]any)
		if body["model"] != "jev-latest" || body["state"] != "the router is 10.0.0.1" || q["type"] != "noul" {
			t.Errorf("body %v", body)
		}
		w.Write([]byte(`{"model":"jev-1.13.0","answers":{"changes":{"type":"noul","noul":0.91},
			"team":{"type":"choice","choice":"ops"},"urgency":{"type":"score","score":1.5}},
			"routing":{"model":"english"},"usage":{"input_tokens":30,"output_tokens":3}}`))
	})
	got, err := c.Ask(context.Background(), "the router is 10.0.0.1",
		map[string]Question{"changes": {Type: Noul, Instructions: "changes?"}})
	if err != nil {
		t.Fatal(err)
	}
	a := got.Answers
	if a["changes"].Noul != 0.91 || a["team"].Choice != "ops" || a["urgency"].Score != 1.5 {
		t.Errorf("answers %+v", got)
	}
	if got.Model != "jev-1.13.0" || got.InputTokens != 30 || got.OutputTokens != 3 {
		t.Errorf("usage %+v", got)
	}
}

func TestChoiceMayComeAsAnObject(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"answers":{"team":{"type":"choice","choice":{"label":"ops","confidence":0.8}}}}`))
	})
	got, err := c.Ask(context.Background(), "x", map[string]Question{"team": {Type: Choice}})
	if err != nil || got.Answers["team"].Choice != "ops" || got.Answers["team"].Confidence != 0.8 {
		t.Errorf("%+v %v", got, err)
	}
}

func TestFailuresAreErrorsNotGuesses(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"status":  func(w http.ResponseWriter, r *http.Request) { http.Error(w, "nope", 500) },
		"garbage": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) },
		"range": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"answers":{"c":{"type":"noul","noul":3}}}`))
		},
		"unknownType": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"answers":{"c":{"type":"poem","poem":"hi"}}}`))
		},
		"slow": func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1500 * time.Millisecond)
			w.Write([]byte(`{"answers":{}}`))
		},
	} {
		c := server(t, h)
		if _, err := c.Ask(context.Background(), "x", map[string]Question{"c": {Type: Noul}}); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
	if _, err := (&Client{}).Ask(context.Background(), "x", nil); err != ErrOff {
		t.Errorf("unconfigured client = %v, want ErrOff", err)
	}
}

func TestErrorsNeverEchoTheKey(t *testing.T) {
	c := server(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key "+strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), http.StatusUnauthorized)
	})
	c.APIKey = "sk-very-secret-value"
	_, err := c.Ask(context.Background(), "x", map[string]Question{"c": {Type: Noul}})
	if err == nil {
		t.Fatal("no error")
	}
	if strings.Contains(err.Error(), "sk-very-secret-value") {
		t.Errorf("error repeats the key: %v", err)
	}
}
