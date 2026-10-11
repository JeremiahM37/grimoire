package memimage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// mapGetter is the settings view the captioner reads.
type mapGetter map[string]string

func (m mapGetter) Get(k string) string { return m[k] }

// fakeVision is an Ollama-shaped captioner. It records how many requests it
// received, what the last one carried, and answers with reply.
func fakeVision(t *testing.T, reply string, status int) (*httptest.Server, *atomic.Int32, *string) {
	t.Helper()
	var hits atomic.Int32
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		last = string(body)
		if status != http.StatusOK {
			http.Error(w, "boom", status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": reply}})
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &last
}

func TestCaptionerIsOffByDefault(t *testing.T) {
	if CaptionerFrom(nil) != nil {
		t.Fatal("nil settings produced a captioner")
	}
	if CaptionerFrom(mapGetter{}) != nil {
		t.Fatal("empty settings produced a captioner")
	}
	// A URL alone is not enough: the model is part of the opt-in.
	if CaptionerFrom(mapGetter{KeyCaptionURL: "http://127.0.0.1:11434"}) != nil {
		t.Fatal("URL without a model produced a captioner")
	}
	// An unknown backend is off, not silently Ollama.
	if CaptionerFrom(mapGetter{KeyCaptionURL: "http://127.0.0.1:1", KeyCaptionModel: "m", KeyCaptionBackend: "anthropic"}) != nil {
		t.Fatal("unknown backend produced a captioner")
	}
	// A URL with credentials or a non-http scheme is refused at configuration.
	for _, bad := range []string{"file:///etc/passwd", "http://user:pw@127.0.0.1:1", "ftp://x"} {
		if CaptionerFrom(mapGetter{KeyCaptionURL: bad, KeyCaptionModel: "m", KeyCaptionAllowPrivate: "1"}) != nil {
			t.Fatalf("invalid url %q produced a captioner", bad)
		}
	}
}

func TestCaptionerOptInCaptionsThePicture(t *testing.T) {
	srv, hits, last := fakeVision(t, "  A dark  rack\nwith two switches. ", http.StatusOK)
	c := CaptionerFrom(mapGetter{KeyCaptionURL: srv.URL, KeyCaptionModel: "llava", KeyCaptionAllowPrivate: "1"})
	if c == nil {
		t.Fatal("configured captioner is nil")
	}
	data := []byte("\x89PNG\r\n\x1a\nfake-but-opaque-bytes")
	got, err := c.Caption(context.Background(), data, FormatPNG)
	if err != nil {
		t.Fatalf("caption: %v", err)
	}
	if got != "A dark rack with two switches." {
		t.Fatalf("caption not cleaned to one line: %q", got)
	}
	if hits.Load() != 1 {
		t.Fatalf("captioner called %d times, want 1", hits.Load())
	}
	var sent struct {
		Model    string `json:"model"`
		Messages []struct {
			Images []string `json:"images"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(*last), &sent); err != nil {
		t.Fatal(err)
	}
	if sent.Model != "llava" || len(sent.Messages) != 1 || len(sent.Messages[0].Images) != 1 {
		t.Fatalf("request shape: %s", *last)
	}
	if sent.Messages[0].Images[0] != base64.StdEncoding.EncodeToString(data) {
		t.Fatal("the picture sent is not the stored bytes")
	}
}

func TestCaptionerOpenAIBackend(t *testing.T) {
	var seenPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{
			map[string]any{"message": map[string]any{"content": "a whiteboard with a diagram"}}}})
	}))
	defer srv.Close()
	c := CaptionerFrom(mapGetter{KeyCaptionBackend: "openai", KeyCaptionURL: srv.URL + "/v1",
		KeyCaptionModel: "gpt-vision", KeyCaptionAllowPrivate: "true"})
	got, err := c.Caption(context.Background(), []byte("GIF89a..."), FormatGIF)
	if err != nil {
		t.Fatal(err)
	}
	if got != "a whiteboard with a diagram" || seenPath != "/v1/chat/completions" {
		t.Fatalf("got %q at %q", got, seenPath)
	}
}

// The API key is read from the environment and sent as a bearer token, and
// only to the configured captioner.
func TestCaptionerSendsEnvKeyAsBearer(t *testing.T) {
	t.Setenv(EnvCaptionAPIKey, "s3cret-test-value")
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"content": "x"}})
	}))
	defer srv.Close()
	c := CaptionerFrom(mapGetter{KeyCaptionURL: srv.URL, KeyCaptionModel: "m", KeyCaptionAllowPrivate: "1"})
	if _, err := c.Caption(context.Background(), []byte("GIF89a"), FormatGIF); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer s3cret-test-value" {
		t.Fatalf("authorization header %q", auth)
	}
}

// The address guard refuses loopback unless the operator opted in. The fake
// server is on loopback, so this is the same refusal a LAN-only deployment
// would get from a private address.
func TestCaptionerRefusesPrivateAddressUnlessAllowed(t *testing.T) {
	srv, hits, _ := fakeVision(t, "should not be reached", http.StatusOK)
	c := CaptionerFrom(mapGetter{KeyCaptionURL: srv.URL, KeyCaptionModel: "m"})
	if c == nil {
		t.Fatal("captioner should be configured; the refusal happens at connect time")
	}
	if _, err := c.Caption(context.Background(), []byte("GIF89a"), FormatGIF); err == nil {
		t.Fatal("loopback captioner reached without the allow setting")
	}
	if hits.Load() != 0 {
		t.Fatalf("refused address still received %d request(s)", hits.Load())
	}
}

// Metadata and link-local addresses are refused even when private networks are
// allowed.
func TestCaptionerAlwaysRefusesLinkLocal(t *testing.T) {
	c := CaptionerFrom(mapGetter{KeyCaptionURL: "http://169.254.169.254", KeyCaptionModel: "m", KeyCaptionAllowPrivate: "1"})
	if c == nil {
		t.Fatal("captioner should be configured")
	}
	if _, err := c.Caption(context.Background(), []byte("GIF89a"), FormatGIF); err == nil {
		t.Fatal("cloud metadata address was reached")
	}
}

// Failure is an error the caller can ignore. An error status, an empty reply
// and a malformed reply all surface as errors and never as an empty caption.
func TestCaptionerFailuresAreErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		reply  string
	}{
		{"server error", http.StatusInternalServerError, ""},
		{"empty text", http.StatusOK, "   \"\"  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _, _ := fakeVision(t, tc.reply, tc.status)
			c := CaptionerFrom(mapGetter{KeyCaptionURL: srv.URL, KeyCaptionModel: "m", KeyCaptionAllowPrivate: "1"})
			got, err := c.Caption(context.Background(), []byte("GIF89a"), FormatGIF)
			if err == nil {
				t.Fatalf("expected an error, got caption %q", got)
			}
		})
	}
}

func TestCleanCaptionBoundsAndNormalises(t *testing.T) {
	long := strings.Repeat("é", MaxCaptionChars+10)
	got, err := CleanCaption(long)
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(got)); n != MaxCaptionChars {
		t.Fatalf("caption is %d runes, want %d", n, MaxCaptionChars)
	}
	if _, err := CleanCaption(" \n\t "); err == nil {
		t.Fatal("blank reply produced a caption")
	}
}
