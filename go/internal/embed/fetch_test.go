package embed

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func writeModel(t *testing.T, dir string, tokenizer, weights string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"tokenizer.json": tokenizer, "model.safetensors": weights,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFindModelUsesTheHuggingFaceCacheLayout(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	t.Setenv("GRIMOIRE_MODEL_DIR", "")

	if got := FindModel("minishlab/potion-base-8M"); got != "" {
		t.Errorf("found %q in an empty cache", got)
	}
	// a cache populated by any other tool must be reused, not duplicated
	dir := filepath.Join(hub, "models--minishlab--potion-base-8M", "snapshots", "abc123")
	writeModel(t, dir, "{}", "weights")
	if got := FindModel("minishlab/potion-base-8M"); got != dir {
		t.Errorf("FindModel = %q, want %q", got, dir)
	}
}

func TestFindModelRejectsAnIncompleteSnapshot(t *testing.T) {
	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	t.Setenv("GRIMOIRE_MODEL_DIR", "")
	dir := filepath.Join(hub, "models--x--y", "snapshots", "abc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// a half-downloaded snapshot must not look usable, or the loader fails
	// later with a confusing parse error instead of falling back cleanly
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := FindModel("x/y"); got != "" {
		t.Errorf("FindModel = %q, want empty for a partial snapshot", got)
	}
	// and a zero-byte file counts as missing
	writeModel(t, dir, "{}", "")
	if got := FindModel("x/y"); got != "" {
		t.Errorf("FindModel = %q, want empty for a zero-byte weight file", got)
	}
}

func TestModelDirOverrideMustBeComplete(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	empty := t.TempDir()
	t.Setenv("GRIMOIRE_MODEL_DIR", empty)
	if got := FindModel("x/y"); got != "" {
		t.Errorf("FindModel = %q, want empty — the override has no model", got)
	}
	// an override that is set but incomplete must NOT silently download into
	// the shared cache: the operator asked for a specific copy, and quietly
	// using a different one hides the misconfiguration
	if _, err := FetchModel("x/y"); err == nil {
		t.Error("FetchModel ignored an incomplete GRIMOIRE_MODEL_DIR")
	}

	writeModel(t, empty, "{}", "weights")
	if got := FindModel("x/y"); got != empty {
		t.Errorf("FindModel = %q, want the override %q", got, empty)
	}
}

func TestFetchModelDownloadsBothFiles(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch filepath.Base(r.URL.Path) {
		case "tokenizer.json":
			_, _ = w.Write([]byte(`{"tok":true}`))
		case "model.safetensors":
			_, _ = w.Write([]byte("binary-weights"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	t.Setenv("GRIMOIRE_MODEL_DIR", "")
	old := HubBase
	HubBase = srv.URL
	t.Cleanup(func() { HubBase = old })

	dir, err := FetchModel("minishlab/potion-base-8M")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "tokenizer.json")); string(got) != `{"tok":true}` {
		t.Errorf("tokenizer = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "model.safetensors")); string(got) != "binary-weights" {
		t.Errorf("weights = %q", got)
	}
	// no .part files left behind
	parts, _ := filepath.Glob(filepath.Join(dir, "*.part"))
	if len(parts) != 0 {
		t.Errorf("leftover partial downloads: %v", parts)
	}
	// and it is found afterwards, so a second start does not re-download
	if FindModel("minishlab/potion-base-8M") != dir {
		t.Error("the downloaded model is not discoverable")
	}
}

func TestFetchModelLeavesNothingBehindOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if filepath.Base(r.URL.Path) == "tokenizer.json" {
			_, _ = w.Write([]byte("{}"))
			return
		}
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	hub := t.TempDir()
	t.Setenv("HF_HUB_CACHE", hub)
	t.Setenv("GRIMOIRE_MODEL_DIR", "")
	old := HubBase
	HubBase = srv.URL
	t.Cleanup(func() { HubBase = old })

	if _, err := FetchModel("x/y"); err == nil {
		t.Fatal("a failed download reported success")
	}
	// the half-fetched directory must not pass as complete, or the next start
	// loads a model with no weights
	if got := FindModel("x/y"); got != "" {
		t.Errorf("FindModel = %q after a failed download", got)
	}
}

func TestEnsureModelDoesNotDownloadWhenNotAllowed(t *testing.T) {
	t.Setenv("HF_HUB_CACHE", t.TempDir())
	t.Setenv("GRIMOIRE_MODEL_DIR", "")
	old := HubBase
	HubBase = "http://127.0.0.1:1" // any request here fails fast
	t.Cleanup(func() { HubBase = old })

	// CLI commands pass false: `grimoire ls` must not block on a 30 MB fetch
	if got := EnsureModel("x/y", false); got != "" {
		t.Errorf("EnsureModel = %q, want empty", got)
	}
}
