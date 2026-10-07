package rerank

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------- settings

type fakeSettings map[string]string

func (f fakeSettings) Get(k string) string { return f[k] }

func TestNewFromSettings(t *testing.T) {
	t.Setenv("HF_HUB_OFFLINE", "")
	data := t.TempDir()
	modelDir := t.TempDir()
	for name, b := range fakeModelFiles() {
		os.WriteFile(filepath.Join(modelDir, name), b, 0o644)
	}
	cases := []struct {
		name     string
		st       fakeSettings
		download bool
		want     string // backend name prefix, "" = nil, "error"
	}{
		{"off", fakeSettings{"rerank": "off"}, true, ""},
		{"auto downloadable", fakeSettings{}, true, "local:" + DefaultModel},
		{"auto nothing available", fakeSettings{}, false, ""},
		{"auto local dir", fakeSettings{"rerank_model": modelDir}, false, "local:" + modelDir},
		{"auto prefers an explicit url", fakeSettings{"rerank_url": "http://x"}, false, "remote:"},
		{"local missing", fakeSettings{"rerank": "local"}, false, "error"},
		{"local downloadable", fakeSettings{"rerank": "LOCAL"}, true, "local:"},
		{"remote", fakeSettings{"rerank": "remote", "rerank_url": "http://x", "rerank_model": "m"}, false, "remote:m"},
		{"remote without url", fakeSettings{"rerank": "remote"}, false, "error"},
		{"bad mode", fakeSettings{"rerank": "sometimes"}, true, "error"},
		{"bad max len", fakeSettings{"rerank_max_len": "lots"}, true, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, err := New(c.st, Options{DataDir: data, AllowDownload: c.download})
			switch {
			case c.want == "error":
				if err == nil {
					t.Fatalf("no error, got %v", r)
				}
			case err != nil:
				t.Fatal(err)
			case c.want == "":
				if r != nil {
					t.Fatalf("got %s, want off", r.Name())
				}
			case r == nil:
				t.Fatalf("got off, want %s", c.want)
			case !strings.HasPrefix(r.Name(), c.want):
				t.Fatalf("got %s, want %s", r.Name(), c.want)
			}
		})
	}
	// max_len flows through, and the model lands under the data dir
	r, err := New(fakeSettings{"rerank_max_len": "128"}, Options{DataDir: data, AllowDownload: true})
	if err != nil {
		t.Fatal(err)
	}
	l := r.(*Local)
	if l.cfg.MaxLen != 128 || l.cfg.CacheDir != filepath.Join(data, "models") {
		t.Fatalf("config %+v", l.cfg)
	}
}
