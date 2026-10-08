package rerank

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Settings is the read side of the settings store (settings.Store satisfies
// it), so this package does not depend on where values come from.
type Settings interface {
	Get(key string) string
}

// Setting keys this package reads; see docs/CONFIG.md.
const (
	SettingMode   = "rerank"         // auto | local | remote | off
	SettingModel  = "rerank_model"   // hub repo id or local directory (local); model name (remote)
	SettingURL    = "rerank_url"     // remote base URL
	SettingAPIKey = "rerank_api_key" // remote bearer token
	SettingMaxLen = "rerank_max_len" // local pair length in tokens
)

// Options carries what settings cannot: where the data directory is and
// whether this process may use the network.
type Options struct {
	// DataDir is the Grimoire data directory (.grimoire); fetched models go
	// in DataDir/models.
	DataDir string
	// AllowDownload lets the local reranker fetch its model on first use.
	// Serving should allow it; one-shot CLI commands should not block on a
	// 90 MB download.
	AllowDownload bool
	// Threads is the local reranker's parallelism; 0 means GOMAXPROCS.
	Threads int
	// HTTPClient is used by the remote reranker; nil means a default client.
	HTTPClient *http.Client
	// Timeout bounds one remote request; 0 means 30s.
	Timeout time.Duration
}

// New builds the reranker the settings select. It returns (nil, nil) when
// reranking is off — including "auto" with nothing available — so callers
// test for nil and keep their first-stage order.
//
//	off     never rerank
//	local   the pure-Go cross-encoder (an error if its model is neither on
//	        disk nor downloadable)
//	remote  a /rerank service at rerank_url
//	auto    remote when rerank_url is set; else local when the model is on
//	        disk or may be downloaded; else off
//
// A local reranker loads (and if need be downloads) its model on its first
// Score, so construction never blocks on the network.
func New(st Settings, opt Options) (Reranker, error) {
	mode := strings.ToLower(strings.TrimSpace(st.Get(SettingMode)))
	model := strings.TrimSpace(st.Get(SettingModel))
	url := strings.TrimSpace(st.Get(SettingURL))

	switch mode {
	case "off", "none", "false", "0", "no":
		return nil, nil
	case "remote":
		return newRemoteFrom(st, opt, url, model)
	case "local":
		return newLocalFrom(st, opt, model, true)
	case "", "auto":
		if url != "" {
			return newRemoteFrom(st, opt, url, model)
		}
		return newLocalFrom(st, opt, model, false)
	default:
		return nil, fmt.Errorf("unknown %s mode %q (want auto, local, remote or off)", SettingMode, mode)
	}
}

func newRemoteFrom(st Settings, opt Options, url, model string) (Reranker, error) {
	if url == "" {
		return nil, fmt.Errorf("%s=remote needs %s", SettingMode, SettingURL)
	}
	if model == DefaultModel {
		model = "" // the local default means nothing to a remote service
	}
	r, err := NewRemote(RemoteConfig{
		URL:     url,
		APIKey:  strings.TrimSpace(st.Get(SettingAPIKey)),
		Model:   model,
		Timeout: opt.Timeout,
		Client:  opt.HTTPClient,
	})
	if err != nil {
		return nil, err // not r: a nil *Remote is a non-nil Reranker
	}
	return r, nil
}

func newLocalFrom(st Settings, opt Options, model string, required bool) (Reranker, error) {
	if model == "" {
		model = DefaultModel
	}
	cacheDir := ""
	if opt.DataDir != "" {
		cacheDir = filepath.Join(opt.DataDir, "models")
	}
	available := FindModel(model, cacheDir) != "" ||
		(opt.AllowDownload && Downloadable(model, cacheDir))
	if !available {
		if required {
			return nil, fmt.Errorf("%w: %s", ErrModelMissing, model)
		}
		return nil, nil
	}
	maxLen := DefaultMaxLen
	if v := strings.TrimSpace(st.Get(SettingMaxLen)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < pairSpecials+2 {
			return nil, fmt.Errorf("%s=%q is not a usable token count", SettingMaxLen, v)
		}
		maxLen = n
	}
	return NewLocal(LocalConfig{
		Model:         model,
		CacheDir:      cacheDir,
		AllowDownload: opt.AllowDownload,
		MaxLen:        maxLen,
		Threads:       opt.Threads,
	}), nil
}
