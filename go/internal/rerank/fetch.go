package rerank

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/embed"
)

// Like the local embedding model, the cross-encoder's weights are a file the
// binary cannot contain, so they are fetched once on first use — into the
// Grimoire data directory, since unlike the embedder's tiny model there is no
// shared cache another tool is likely to have filled — and checked before
// they are trusted.
//
// Every file is verified: the default model against checksums pinned to a
// fixed revision here, so a changed upstream file is a loud failure rather
// than a silently different ranking; any other model against the hashes the
// hub reports for the file (X-Linked-Etag: sha256 for large files, the git
// blob id for small ones). A download is written to a .part file and only
// renamed into place once it verifies, so an interrupted or corrupt fetch
// never leaves a file that looks complete.

// modelFiles are the files the loader reads.
var modelFiles = []string{"config.json", "tokenizer.json", "model.safetensors"}

type pinnedModel struct {
	revision string
	sha256   map[string]string
}

// pinned are models whose files are verified against known checksums. The
// repo has since been renamed upstream; the old name redirects, and the
// revision pins the bytes either way.
var pinned = map[string]pinnedModel{
	DefaultModel: {
		revision: "233902d25c440f23af6f7d6e94d2946bac0bee0a",
		sha256: map[string]string{
			"config.json":       "380e02c93f431831be65d99a4e7e5f67c133985bf2e77d9d4eba46847190bacc",
			"tokenizer.json":    "d241a60d5e8f04cc1b2b3e9ef7a4921b27bf526d9f6050ab90f9267a1f9e5c66",
			"model.safetensors": "821d1aa69520101d6e0737f78a042ae25b19e5cb9160701909d10434f4aeb0ae",
		},
	},
}

// ErrModelMissing reports a model that is not on disk and may not be
// downloaded.
var ErrModelMissing = errors.New("rerank model is not downloaded")

// isLocalPath reports whether a model setting names a directory rather than a
// hub repo id.
func isLocalPath(model string) bool {
	if filepath.IsAbs(model) || strings.HasPrefix(model, ".") || strings.HasPrefix(model, "~") {
		return true
	}
	st, err := os.Stat(model)
	return err == nil && st.IsDir() && complete(model)
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[1:])
		}
	}
	return p
}

// ModelDir is where a hub model is stored under cacheDir.
func ModelDir(cacheDir, model string) string {
	return filepath.Join(cacheDir, strings.ReplaceAll(model, "/", "--"))
}

// FindModel returns the directory holding a usable copy of model, or "".
func FindModel(model, cacheDir string) string {
	if isLocalPath(model) {
		dir := expandHome(model)
		if complete(dir) {
			return dir
		}
		return ""
	}
	if cacheDir == "" {
		return ""
	}
	if dir := ModelDir(cacheDir, model); complete(dir) {
		return dir
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

// offline reports whether the environment forbids hub downloads, honouring
// the HuggingFace convention so an air-gapped host is one variable.
func offline() bool {
	v := strings.ToLower(os.Getenv("HF_HUB_OFFLINE"))
	return v == "1" || v == "true" || v == "yes"
}

// Downloadable reports whether a missing model could be fetched: it names a
// hub repo, there is somewhere to put it, and nothing forbids the network.
// It does not touch the network.
func Downloadable(model, cacheDir string) bool {
	return !isLocalPath(model) && cacheDir != "" && !offline()
}

// ensureModel finds a model or, when allowed, downloads it.
func ensureModel(model, cacheDir string, allowDownload bool) (string, error) {
	if dir := FindModel(model, cacheDir); dir != "" {
		return dir, nil
	}
	if isLocalPath(model) {
		return "", fmt.Errorf("%s is missing one of %v", model, modelFiles)
	}
	if !allowDownload || !Downloadable(model, cacheDir) {
		return "", fmt.Errorf("%w: %s (allow downloads or point rerank_model at a local copy)",
			ErrModelMissing, model)
	}
	log.Printf("downloading rerank model %s (~90 MB, once)", model)
	dir, err := FetchModel(model, cacheDir)
	if err != nil {
		return "", err
	}
	log.Printf("rerank model ready at %s", dir)
	return dir, nil
}

// FetchModel downloads a hub model into cacheDir and returns its directory.
// It is a no-op for files already present.
func FetchModel(model, cacheDir string) (string, error) {
	if cacheDir == "" {
		return "", fmt.Errorf("no directory to store the rerank model in")
	}
	dir := ModelDir(cacheDir, model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	pin, isPinned := pinned[model]
	revision := "main"
	if isPinned {
		revision = pin.revision
	}
	client := &http.Client{Timeout: 10 * time.Minute}
	for _, f := range modelFiles {
		dest := filepath.Join(dir, f)
		if st, err := os.Stat(dest); err == nil && st.Size() > 0 {
			continue
		}
		url := fmt.Sprintf("%s/%s/resolve/%s/%s", strings.TrimRight(embed.HubBase, "/"), model, revision, f)
		if err := fetchFile(client, url, dest, pin.sha256[f]); err != nil {
			return "", fmt.Errorf("downloading %s: %w", f, err)
		}
	}
	return dir, nil
}

var hexDigest = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// fetchFile downloads url to dest, verifying it against wantSHA256 when set,
// otherwise against the digest the hub reports.
func fetchFile(client *http.Client, url, dest, wantSHA256 string) error {
	// The hub's digest header is on the redirect it answers with, not on the
	// CDN response the redirect leads to, so collect it from every hop.
	var linked string
	c := *client
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if r := req.Response; r != nil && linked == "" {
			linked = digestHeader(r.Header)
		}
		return nil
	}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	if linked == "" {
		linked = digestHeader(resp.Header)
	}

	var h hash.Hash
	var want string
	gitBlob := false
	switch {
	case wantSHA256 != "":
		h, want = sha256.New(), wantSHA256
	case len(linked) == 64:
		h, want = sha256.New(), linked
	case len(linked) == 40:
		// a small file's ETag is its git blob id: sha1("blob <len>\0" + data)
		if resp.ContentLength < 0 {
			return fmt.Errorf("%s: cannot verify a git blob id without a length", url)
		}
		h, want, gitBlob = sha1.New(), linked, true
		fmt.Fprintf(h, "blob %s\x00", strconv.FormatInt(resp.ContentLength, 10))
	default:
		log.Printf("rerank model: %s carries no checksum; accepting it unverified", url)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var w io.Writer = f
	if h != nil {
		w = io.MultiWriter(f, h)
	}
	n, err := io.Copy(w, resp.Body)
	if err == nil && gitBlob && n != resp.ContentLength {
		err = fmt.Errorf("short read: %d of %d bytes", n, resp.ContentLength)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && h != nil {
		if got := hex.EncodeToString(h.Sum(nil)); got != want {
			err = fmt.Errorf("checksum mismatch: got %s, want %s", got, want)
		}
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func digestHeader(h http.Header) string {
	for _, k := range []string{"X-Linked-Etag", "Etag"} {
		v := strings.ToLower(strings.Trim(strings.TrimPrefix(h.Get(k), "W/"), `"`))
		if hexDigest.MatchString(v) {
			return v
		}
	}
	return ""
}
