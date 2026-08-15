package embed

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The local embedding model is a file, not code, so the binary cannot contain
// it — but a build that merely *supports* a model nobody can obtain is not the
// same product. The Python implementation downloaded it through
// huggingface_hub on first use; this does the same with two plain GETs, so a
// fresh install reaches the benchmarked configuration without a separate
// toolchain.
//
// Downloads are skipped entirely when GRIMOIRE_LOCAL_EMBED=off, and the files
// land in the standard HuggingFace cache layout so an existing cache (from any
// tool) is reused rather than duplicated.

// modelFiles are the only two files the loader reads.
var modelFiles = []string{"tokenizer.json", "model.safetensors"}

// HubBase is the download host, overridable for a mirror or an air-gapped
// registry.
var HubBase = envOr("HF_ENDPOINT", "https://huggingface.co")

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// CacheDir is where a model's files live: the HuggingFace cache layout, so a
// cache populated by any other tool is found and reused.
func CacheDir(name string) string {
	hub := os.Getenv("HF_HUB_CACHE")
	if hub == "" {
		if home := os.Getenv("HF_HOME"); home != "" {
			hub = filepath.Join(home, "hub")
		} else {
			h, err := os.UserHomeDir()
			if err != nil {
				return ""
			}
			hub = filepath.Join(h, ".cache", "huggingface", "hub")
		}
	}
	repo := "models--" + strings.ReplaceAll(name, "/", "--")
	return filepath.Join(hub, repo, "snapshots", "main")
}

// FindModel returns a directory holding the model's files, or "" if none does.
// GRIMOIRE_MODEL_DIR wins so an operator can point at a copy they manage.
func FindModel(name string) string {
	if dir := os.Getenv("GRIMOIRE_MODEL_DIR"); dir != "" {
		if complete(dir) {
			return dir
		}
		return ""
	}
	hub := filepath.Dir(filepath.Dir(CacheDir(name))) // …/models--org--name
	hits, err := filepath.Glob(filepath.Join(hub, "snapshots", "*"))
	if err != nil {
		return ""
	}
	for _, dir := range hits {
		if complete(dir) {
			return dir
		}
	}
	return ""
}

func complete(dir string) bool {
	for _, f := range modelFiles {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil || st.Size() == 0 {
			return false
		}
	}
	return true
}

// FetchModel downloads a model into the cache and returns its directory. It is
// a no-op when the files are already there, so calling it on every start is
// cheap and idempotent.
func FetchModel(name string) (string, error) {
	if dir := FindModel(name); dir != "" {
		return dir, nil
	}
	if dir := os.Getenv("GRIMOIRE_MODEL_DIR"); dir != "" {
		return "", fmt.Errorf("GRIMOIRE_MODEL_DIR=%s is missing %v", dir, modelFiles)
	}
	dir := CacheDir(name)
	if dir == "" {
		return "", fmt.Errorf("no cache directory available")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, f := range modelFiles {
		dest := filepath.Join(dir, f)
		if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
			continue
		}
		url := fmt.Sprintf("%s/%s/resolve/main/%s", HubBase, name, f)
		if err := download(client, url, dest); err != nil {
			return "", fmt.Errorf("downloading %s: %w", f, err)
		}
	}
	return dir, nil
}

func download(client *http.Client, url, dest string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	// via a temp file, so an interrupted download never leaves a truncated
	// model that later looks complete
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

// EnsureModel finds or downloads the model, logging what it is doing when a
// download is needed (a silent 30 MB fetch on first boot would be worse than a
// noisy one). It returns "" when the model is unavailable, which the caller
// treats as "fall back to the hashing embedder".
func EnsureModel(name string, allowDownload bool) string {
	if dir := FindModel(name); dir != "" {
		return dir
	}
	if !allowDownload {
		return ""
	}
	log.Printf("downloading local embedding model %s (~30 MB, once)", name)
	dir, err := FetchModel(name)
	if err != nil {
		log.Printf("could not fetch %s: %v — falling back to the hashing "+
			"embedder (set GRIMOIRE_LOCAL_EMBED=off to stop trying)", name, err)
		return ""
	}
	log.Printf("local embedding model ready at %s", dir)
	return dir
}
