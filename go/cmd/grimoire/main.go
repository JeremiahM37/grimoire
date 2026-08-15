// Command grimoire runs the personal context server.
//
// The whole point of the Go build: one static binary, no runtime, no virtualenv.
// Configuration matches the Python service's GRIMOIRE_* variables so a
// deployment can switch implementations without touching its unit file.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/api"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
	"github.com/JeremiahM37/grimoire/go/internal/watcher"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "grimoire:", err)
		os.Exit(1)
	}
}

func run() error {
	vaultPath := envOr("GRIMOIRE_VAULT", filepath.Join(os.Getenv("HOME"), "notes"))
	port := envOr("GRIMOIRE_PORT", "9111")
	webDir := envOr("GRIMOIRE_WEB_DIR", "")

	v, err := vault.New(vaultPath)
	if err != nil {
		return fmt.Errorf("opening vault: %w", err)
	}
	if err := os.MkdirAll(v.Root, 0o755); err != nil {
		return err
	}
	grimoireDir := filepath.Join(v.Root, ".grimoire")
	database, err := db.Open(filepath.Join(grimoireDir, "index.db"))
	if err != nil {
		return fmt.Errorf("opening index: %w", err)
	}
	defer database.Close()

	vaultSecrets := secrets.New(grimoireDir)
	store := settings.New(grimoireDir)
	emb := newEmbedder(store)
	ix := index.New(database, v, emb)

	log.Printf("indexing vault %s with %s", v.Root, emb.Signature())
	n, err := ix.Reindex()
	if err != nil {
		return fmt.Errorf("reindexing: %w", err)
	}
	log.Printf("indexed %d notes", n)

	srv := &http.Server{
		Addr: ":" + port,
		Handler: (&api.Server{
			Index:     ix,
			Vault:     v,
			Settings:  settings.New(grimoireDir),
			History:   history.New(grimoireDir),
			Secrets:   vaultSecrets,
			Broker:    secrets.NewBroker(vaultSecrets, database),
			CRDT:      crdtstore.New(grimoireDir),
			SyncPeer:  os.Getenv("GRIMOIRE_SYNC_PEER"),
			WebDir:    webDir,
			PluginDir: envOr("GRIMOIRE_PLUGIN_DIR", "plugins"),
			DailyDir:  envOr("GRIMOIRE_DAILY_DIR", "journal"),
			InboxDir:  envOr("GRIMOIRE_INBOX_DIR", "inbox"),
		}).Routes(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Graceful shutdown: an in-flight reindex or write should finish rather
	// than leaving a half-written index behind.
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("shutdown: %v", err)
		}
		close(done)
	}()

	// watch for edits made outside grimoire (another editor, a sync client)
	watch := watcher.New(v, ix, 0)
	if os.Getenv("GRIMOIRE_NO_WATCHER") == "" {
		if err := watch.Start(); err != nil {
			log.Printf("watcher unavailable (%v) — external edits need a manual reindex", err)
		} else {
			defer watch.Stop()
		}
	}

	log.Printf("listening on :%s", port)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-done
	return nil
}

// newEmbedder builds the backend ladder, in the same precedence the Python
// implementation uses: an explicit OpenAI-compatible endpoint, then Ollama,
// then a local model, then the hasher.
//
// The hasher is always last so the server starts and indexes no matter what is
// missing — being unable to embed must degrade retrieval, not prevent startup.
func newEmbedder(st *settings.Store) index.Embedder {
	chain := &embed.Chain{}
	model := st.Get("embed_model")

	if base := st.Get("embed_base_url"); base != "" {
		key := st.Get("embed_api_key")
		if key == "" {
			key = st.Get("llm_api_key")
		}
		chain.Backends = append(chain.Backends, embed.NewOpenAI(base, key, model))
	}
	if url := st.Get("ollama_url"); url != "" {
		chain.Backends = append(chain.Backends, embed.NewOllama(url, model))
	}
	if !strings.EqualFold(st.Get("local_embed"), "off") {
		name := st.Get("local_embed_model")
		if dir := modelDir(name); dir != "" {
			if m, err := embed.LoadModel2Vec(dir, name); err == nil {
				chain.Backends = append(chain.Backends, m)
			} else {
				log.Printf("local embedding model unavailable: %v", err)
			}
		} else {
			// silence here is dangerous: the hasher below still yields a
			// server that starts, indexes, and answers — with far worse
			// retrieval, and nothing in the output says why
			log.Printf("local embedding model %q not found in the HuggingFace "+
				"cache; set GRIMOIRE_MODEL_DIR to its snapshot directory", name)
		}
	}
	chain.Backends = append(chain.Backends, embed.Hash{})
	return chain
}

// modelDir locates a model2vec snapshot. GRIMOIRE_MODEL_DIR wins; otherwise we
// look where `huggingface_hub` puts it, because that is where the Python
// implementation's automatic download lands — a vault indexed by one build
// should not silently fall back to hash embeddings under the other.
func modelDir(name string) string {
	if dir := os.Getenv("GRIMOIRE_MODEL_DIR"); dir != "" {
		return dir
	}
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
	// snapshots are named by commit hash; any of them is the right model, so
	// take whichever the glob finds first rather than tracking revisions
	hits, err := filepath.Glob(filepath.Join(hub, repo, "snapshots", "*"))
	if err != nil {
		return ""
	}
	for _, dir := range hits {
		if _, err := os.Stat(filepath.Join(dir, "tokenizer.json")); err == nil {
			return dir
		}
	}
	return ""
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
