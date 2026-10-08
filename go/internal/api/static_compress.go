package api

import (
	"bytes"
	"compress/gzip"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
)

// Static assets used to go out uncompressed (a 550 KB script over a phone's
// radio).  They are compressed once per (file, mtime, size) and kept in memory;
// the set is the built shell, a few hundred KB.
type gzEntry struct {
	data []byte
}

var (
	gzCache   sync.Map // key -> *gzEntry
	gzTypable = map[string]bool{".js": true, ".css": true, ".html": true, ".svg": true, ".json": true, ".webmanifest": true, ".map": false}
)

// serveCompressed answers GET/HEAD for a compressible file under root when the
// client accepts gzip.  It reports whether it handled the request.
func serveCompressed(w http.ResponseWriter, r *http.Request, root string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		return false
	}
	name := path.Clean("/" + r.URL.Path)
	if name == "/" {
		name = "/index.html"
	}
	ext := strings.ToLower(path.Ext(name))
	if !gzTypable[ext] {
		return false
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Stat(full)
	if err != nil || info.IsDir() || info.Size() < 512 {
		return false
	}
	key := full + "|" + info.ModTime().String() + "|" + string(rune(info.Size()))
	v, ok := gzCache.Load(key)
	if !ok {
		raw, err := os.ReadFile(full)
		if err != nil {
			return false
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		v = &gzEntry{data: buf.Bytes()}
		gzCache.Store(key, v)
	}
	w.Header().Set("Content-Encoding", "gzip")
	w.Header().Add("Vary", "Accept-Encoding")
	if ct := mime.TypeByExtension(ext); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	http.ServeContent(w, r, "", info.ModTime(), bytes.NewReader(v.(*gzEntry).data))
	return true
}
