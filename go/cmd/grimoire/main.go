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
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/ai"
	"github.com/JeremiahM37/grimoire/go/internal/api"
	"github.com/JeremiahM37/grimoire/go/internal/auth"
	"github.com/JeremiahM37/grimoire/go/internal/connectors"
	"github.com/JeremiahM37/grimoire/go/internal/crdtstore"
	"github.com/JeremiahM37/grimoire/go/internal/db"
	"github.com/JeremiahM37/grimoire/go/internal/embed"
	"github.com/JeremiahM37/grimoire/go/internal/history"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/readlog"
	"github.com/JeremiahM37/grimoire/go/internal/secrets"
	"github.com/JeremiahM37/grimoire/go/internal/settings"
	gsync "github.com/JeremiahM37/grimoire/go/internal/sync"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
	"github.com/JeremiahM37/grimoire/go/internal/watcher"
	"github.com/JeremiahM37/grimoire/go/internal/websearch"
)

func main() {
	args := os.Args[1:]
	if handled, code := runCLI(args); handled {
		os.Exit(code)
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "grimoire:", err)
		os.Exit(1)
	}
}

// env is everything a command needs: the vault, its index, and the same HTTP
// handler the server exposes. Built once, by serve and by every CLI command, so
// the two can never be configured differently.
type env struct {
	vault    *vault.Vault
	index    *index.Index
	db       *db.DB
	settings *settings.Store
	sync     *gsync.Client
	server   *api.Server
	handler  http.Handler
	embedder index.Embedder
	auth     *auth.Store
	dailyDir string
	inboxDir string
}

func (e *env) close() {
	if e.db != nil {
		e.db.Close()
	}
}

func newEnv(fetchModel bool) (*env, error) {
	vaultPath := envOr("GRIMOIRE_VAULT", filepath.Join(os.Getenv("HOME"), "notes"))
	v, err := vault.New(vaultPath)
	if err != nil {
		return nil, fmt.Errorf("opening vault: %w", err)
	}
	if err := os.MkdirAll(v.Root, 0o755); err != nil {
		return nil, err
	}
	grimoireDir := filepath.Join(v.Root, ".grimoire")
	database, err := db.Open(filepath.Join(grimoireDir, "index.db"))
	if err != nil {
		return nil, fmt.Errorf("opening index: %w", err)
	}

	vaultSecrets := secrets.New(grimoireDir)
	accounts := auth.New(database)
	reads := readlog.New(database)
	connectorStore := connectors.NewStore(database)
	store := settings.New(grimoireDir)
	emb := newEmbedder(store, fetchModel)
	ix := index.New(database, v, emb)
	crdt := crdtstore.New(grimoireDir)
	syncer := gsync.New(ix, v, crdt)

	srv := &api.Server{
		Index:      ix,
		Vault:      v,
		Settings:   store,
		History:    history.New(grimoireDir),
		Secrets:    vaultSecrets,
		Broker:     secrets.NewBroker(vaultSecrets, database),
		CRDT:       crdt,
		AI:         ai.New(store, vaultSecrets.Get),
		Auth:       accounts,
		Reads:      reads,
		Connectors: connectorStore,
		Web: &websearch.Client{
			Settings: store, Secrets: vaultSecrets.Get,
			// The same outbound guard the credential broker uses: the URL
			// comes from whoever is asking, so private ranges and cloud
			// metadata are refused, on the address the socket will use.
			HTTP: &http.Client{
				Timeout:   45 * time.Second,
				Transport: secrets.GuardedTransport(secrets.AllowPrivateFromEnv()),
			},
		},
		Sync:         syncer,
		SyncPeer:     os.Getenv("GRIMOIRE_SYNC_PEER"),
		SyncToken:    os.Getenv("GRIMOIRE_SYNC_TOKEN"),
		SyncInterval: atoiOr(os.Getenv("GRIMOIRE_SYNC_INTERVAL"), 0),
		WebDir:       envOr("GRIMOIRE_WEB_DIR", ""),
		AuthToken:    os.Getenv("GRIMOIRE_AUTH_TOKEN"),
		AdminToken:   os.Getenv("GRIMOIRE_ADMIN_TOKEN"),
		FrameOptions: envOr("GRIMOIRE_FRAME_OPTIONS", "SAMEORIGIN"),
		PluginDir:    envOr("GRIMOIRE_PLUGIN_DIR", "plugins"),
		DailyDir:     envOr("GRIMOIRE_DAILY_DIR", "journal"),
		InboxDir:     envOr("GRIMOIRE_INBOX_DIR", "inbox"),
	}
	// The index writes each row's space, and the server owns the mapping from
	// path to space. Wiring it after construction keeps the index free of any
	// dependency on accounts: with none configured it stamps everything
	// "commons", exactly as a single-user vault always has.
	ix.Spaces = srv
	// The runner writes through the server, which owns note writing, indexing
	// and spaces — so a pulled document is an ordinary note the moment it
	// lands, with nothing special about it downstream.
	srv.Runner = &connectors.Runner{
		Store: connectorStore, Writer: srv,
		Secrets:    api.SecretsForConnectors{Server: srv},
		Identities: accounts,
		Client:     connectorClient(),
	}

	return &env{vault: v, index: ix, db: database, settings: store, sync: syncer,
		server: srv, handler: srv.Routes(), embedder: emb, auth: accounts,
		dailyDir: srv.DailyDir, inboxDir: srv.InboxDir}, nil
}

// openEnv is newEnv for CLI commands: it also makes sure the index exists, so
// `grimoire search` works on a vault that has never been served.
func openEnv() (*env, error) {
	e, err := newEnv(false)
	if err != nil {
		return nil, err
	}
	n, err := e.index.DB.Count("SELECT COUNT(*) FROM notes")
	if err != nil {
		e.close()
		return nil, err
	}
	if n == 0 {
		if _, err := e.index.Reindex(); err != nil {
			e.close()
			return nil, err
		}
	}
	return e, nil
}

func run(args []string) error {
	e, err := newEnv(true)
	if err != nil {
		return err
	}
	defer e.close()

	port := envOr("GRIMOIRE_PORT", "9111")
	if v, ok := flagValue(args, "--port"); ok {
		port = v
	}
	// GRIMOIRE_HOST has been documented since before the rewrite but was never
	// read, so the server always bound every interface. That is the wrong
	// default to pair with an optional auth token: on a machine with any
	// non-trusted network attached, the secrets routes were reachable from it.
	host := envOr("GRIMOIRE_HOST", "")
	if v, ok := flagValue(args, "--host"); ok {
		host = v
	}
	if e.server.AuthToken == "" && host == "" {
		if e.server.AdminToken == "" && (e.auth == nil || !e.auth.Enabled()) {
			log.Printf("WARNING: serving on all interfaces with no GRIMOIRE_AUTH_TOKEN " +
				"and no GRIMOIRE_ADMIN_TOKEN — anyone who can reach this port can read " +
				"notes AND drive the secret vault, accounts and connectors")
		} else {
			log.Printf("serving on all interfaces with no GRIMOIRE_AUTH_TOKEN: notes and " +
				"retrieval are open to anyone who can reach this port; the administrative " +
				"surface is gated")
		}
	}

	// Unattended unlock, when the deployment has opted into one. Done before
	// the listener opens so the broker is never briefly locked to an agent
	// that reconnects the moment the port answers.
	if path := os.Getenv("GRIMOIRE_VAULT_PASSPHRASE_FILE"); path != "" {
		switch err := e.server.Secrets.UnlockFromFile(path); {
		case err == nil:
			log.Printf("secret vault unlocked from %s", path)
		case errors.Is(err, secrets.ErrNotInit):
			log.Printf("GRIMOIRE_VAULT_PASSPHRASE_FILE is set but no vault exists yet — " +
				"initialize one and it will unlock on the next start")
		default:
			// Not fatal: notes must keep serving when only the broker is
			// misconfigured. Loud, because everything using a credential is
			// about to fail with 423.
			log.Printf("WARNING: secret vault auto-unlock failed (%v) — the broker is locked", err)
		}
	}

	// Incremental: a restart re-reads only what changed on disk, and
	// re-embeds only that. A full rebuild still happens when the embedding
	// model or the row shape changes, because rows from two models are not
	// comparable. See internal/index/sync.go.
	log.Printf("indexing vault %s with %s", e.vault.Root, e.embedder.Signature())
	stats, err := e.index.Sync()
	if err != nil {
		return fmt.Errorf("indexing: %w", err)
	}
	log.Printf("indexed %s", stats)

	v, ix, syncer := e.vault, e.index, e.sync
	syncPeer, syncToken, syncInterval := e.server.SyncPeer, e.server.SyncToken, e.server.SyncInterval
	_ = ix

	srv := &http.Server{
		Addr:              net.JoinHostPort(host, port),
		Handler:           e.handler,
		ReadHeaderTimeout: 10 * time.Second,
		// A slow or stalled client must not be able to hold a connection (and,
		// with SetMaxOpenConns(1) underneath, the database behind it) open
		// indefinitely. ReadTimeout is generous because vault import and audio
		// upload send large bodies; there is no streaming response surface, so
		// WriteTimeout can be bounded too.
		ReadTimeout:    5 * time.Minute,
		WriteTimeout:   5 * time.Minute,
		IdleTimeout:    2 * time.Minute,
		MaxHeaderBytes: 1 << 20,
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

	// periodic sync with a peer, when one is configured
	if syncPeer != "" && syncInterval > 0 {
		log.Printf("syncing with %s every %ds", syncPeer, syncInterval)
		go syncer.Loop(syncPeer, syncToken,
			time.Duration(syncInterval)*time.Second, done)
	}

	// The read-audit writer, and its retention. An audit trail that grows
	// forever becomes its own liability: a permanent list of who looked at
	// which sensitive document. GRIMOIRE_READ_AUDIT_DAYS=0 keeps everything.
	if e.server.Reads != nil {
		e.server.Reads.Start()
		defer e.server.Reads.Close()
		days := atoiOr(os.Getenv("GRIMOIRE_READ_AUDIT_DAYS"), readlog.RetentionDays)
		if os.Getenv("GRIMOIRE_READ_AUDIT_DAYS") == "0" {
			days = 0
		}
		if n, err := e.server.Reads.Prune(days); err != nil {
			log.Printf("pruning read audit: %v", err)
		} else if n > 0 {
			log.Printf("pruned %d read-audit records older than %d days", n, days)
		}
	}

	// Dead sessions accumulate one row per login forever otherwise.
	if e.auth != nil {
		if err := e.auth.PurgeExpiredSessions(); err != nil {
			log.Printf("purging expired sessions: %v", err)
		}
	}

	// Connectors on their schedules. One goroutine, one connector at a time:
	// these are network-bound, and a self-hosted instance would rather be
	// polite to the systems it pulls from than fast.
	if e.server.Runner != nil {
		go e.server.Runner.Loop(30*time.Second, done)
	}

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
func newEmbedder(st *settings.Store, download bool) index.Embedder {
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
		// download only when serving: a CLI command should not block on a
		// 30 MB fetch, and `grimoire ls` should not print a warning either
		if dir := embed.EnsureModel(name, download); dir != "" {
			if m, err := embed.LoadModel2Vec(dir, name); err == nil {
				chain.Backends = append(chain.Backends, m)
			} else {
				log.Printf("local embedding model unavailable: %v", err)
			}
		}
	}
	chain.Backends = append(chain.Backends, embed.Hash{})
	return chain
}

// atoiOr parses an integer setting, falling back rather than failing startup on
// a typo in a unit file.
func atoiOr(s string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(s)); err == nil {
		return n
	}
	return def
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// connectorClient is the HTTP client connectors pull with.
//
// Separate from the credential broker's guarded client on purpose: the broker
// refuses private addresses because an agent chooses its target, while a
// connector's target is configured by an administrator — and self-hosted
// Confluence, Jira and GitHub Enterprise all live on private networks. The
// timeout is generous because these are paged APIs on other people's servers.
func connectorClient() *http.Client {
	return &http.Client{Timeout: 90 * time.Second}
}
