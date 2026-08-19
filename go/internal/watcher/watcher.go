// Package watcher is the "edit anywhere" guarantee.
//
// Port of server/watcher.py. It watches the vault for .md changes made OUTSIDE
// grimoire (another editor, vim, git pull, a sync client, another device) and
// reconciles the index. Debounced, so a burst of saves collapses into one pass.
//
// The critical property, learned the hard way in the Python implementation:
// indexing a note READS its file, and a naive watcher treats that read as a
// change. That re-queues the note that was just indexed, which reads it again —
// a self-feeding loop that pins a core forever and leaves readers seeing notes
// mid-rewrite. Only real writes, renames and deletes may reconcile.
//
// fsnotify already reports only write/create/rename/remove/chmod (never opens
// or reads), so the loop cannot arise here — but CHMOD is still filtered out,
// since a metadata touch is not a content change.
package watcher

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Reconciler is the subset of the index a watcher needs.
type Reconciler interface {
	Upsert(rel string) (*vault.Note, error)
	Remove(rel string) error
}

// Watcher debounces filesystem events into single-note upserts and removes.
type Watcher struct {
	vault    *vault.Vault
	index    Reconciler
	debounce time.Duration

	mu      sync.Mutex
	pending map[string]struct{}
	timer   *time.Timer

	fsw  *fsnotify.Watcher
	done chan struct{}
}

func New(v *vault.Vault, ix Reconciler, debounce time.Duration) *Watcher {
	if debounce <= 0 {
		debounce = 600 * time.Millisecond
	}
	return &Watcher{
		vault: v, index: ix, debounce: debounce,
		pending: map[string]struct{}{}, done: make(chan struct{}),
	}
}

// relevant filters events down to real note files. Reserved dirs hold the index
// and CRDT state — reacting to those would be its own feedback loop.
func relevant(path string) bool {
	base := filepath.Base(path)
	if !strings.HasSuffix(base, ".md") || strings.HasSuffix(base, ".tmp") {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(path), "/") {
		for _, r := range vault.ReservedDirs {
			if part == r {
				return false
			}
		}
	}
	return true
}

// Start begins watching. Directories are added recursively, and new ones are
// picked up as they appear.
func (w *Watcher) Start() error {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return err
	}
	w.fsw = fsw
	if err := w.addTree(w.vault.Root); err != nil {
		fsw.Close()
		return err
	}
	go w.loop()
	log.Printf("vault watcher started on %s", w.vault.Root)
	return nil
}

func (w *Watcher) addTree(root string) error {
	// Following the same links the indexer follows: a linked directory whose
	// edits are never noticed is worse than one that is not indexed at all,
	// because search would keep answering from a stale copy.
	return vault.WalkTree(root, w.vault.Follow, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		for _, r := range vault.ReservedDirs {
			if d.Name() == r {
				return filepath.SkipDir
			}
		}
		return w.fsw.Add(path)
	})
}

func (w *Watcher) loop() {
	for {
		select {
		case <-w.done:
			return
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return
			}
			// a new directory must start being watched, or notes created inside
			// it are invisible until the next restart
			if ev.Op&(fsnotify.Create) != 0 {
				if info, err := os.Stat(ev.Name); err == nil && info.IsDir() {
					_ = w.addTree(ev.Name)
					continue
				}
			}
			// CHMOD is a metadata touch, not a content change
			if ev.Op == fsnotify.Chmod || !relevant(ev.Name) {
				continue
			}
			w.queue(ev.Name)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return
			}
			log.Printf("watcher: %v", err)
		}
	}
}

func (w *Watcher) queue(path string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending[path] = struct{}{}
	if w.timer == nil {
		w.timer = time.AfterFunc(w.debounce, w.flush)
	}
}

func (w *Watcher) flush() {
	w.mu.Lock()
	paths := make([]string, 0, len(w.pending))
	for p := range w.pending {
		paths = append(paths, p)
	}
	w.pending = map[string]struct{}{}
	w.timer = nil
	w.mu.Unlock()

	for _, abs := range paths {
		rel, err := w.vault.RelOf(abs)
		if err != nil {
			continue
		}
		if _, statErr := os.Stat(abs); statErr == nil {
			if _, err := w.index.Upsert(rel); err != nil {
				log.Printf("watcher: upsert %s: %v", rel, err)
			}
			continue
		}
		if err := w.index.Remove(rel); err != nil {
			log.Printf("watcher: remove %s: %v", rel, err)
		}
	}
}

// Stop ends watching and releases the inotify handle.
func (w *Watcher) Stop() {
	select {
	case <-w.done:
		return // already stopped
	default:
		close(w.done)
	}
	if w.fsw != nil {
		_ = w.fsw.Close()
	}
	w.mu.Lock()
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
	w.mu.Unlock()
}
