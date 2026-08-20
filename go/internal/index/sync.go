package index

import (
	"fmt"
	"math"

	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// Bringing the index up to date without rebuilding it.
//
// Serving used to start with a full Reindex: every table dropped, every note
// re-read, and — the expensive part — every chunk re-embedded. With a local
// embedder that was 24s for 50,000 notes; with a remote one it is far worse,
// because embedding is a network round trip per note (this homelab measures
// ~100ms/note against Ollama, so ~85 minutes for the same vault). Restarting a
// service should not cost that, and a crash-loop should not multiply it.
//
// Nothing about the vault requires it either: the index already stores each
// note's mtime and content hash, and the vast majority of notes are unchanged
// between one start and the next. Sync reads only what changed.
//
// The signature is what keeps that safe. Embeddings are only comparable to
// others produced by the same model, and the row shape is only comparable
// within one schema version, so when either changes the incremental path is
// abandoned and a full rebuild runs. Skipping that check would leave a vault
// silently half-embedded by two different models — a corpus that still answers,
// with quietly wrong rankings.

// indexSchemaVersion is bumped when the shape of an indexed row changes in a
// way that makes existing rows unusable.
const indexSchemaVersion = "1"

// SyncStats reports what a sync did, for the startup log and for tests that
// need to assert the fast path was actually taken.
type SyncStats struct {
	Added       int
	Updated     int
	Unchanged   int
	Removed     int
	FullRebuild bool
}

func (s SyncStats) String() string {
	if s.FullRebuild {
		return fmt.Sprintf("%d notes (full rebuild)", s.Added)
	}
	return fmt.Sprintf("%d notes (%d new, %d changed, %d removed, %d unchanged)",
		s.Added+s.Updated+s.Unchanged, s.Added, s.Updated, s.Removed, s.Unchanged)
}

// Signature identifies the embedding model and row shape the index was built
// with. Rows built under a different one cannot be compared to new ones.
func (ix *Index) Signature() string {
	return ix.Emb.Signature() + "|v" + indexSchemaVersion
}

// RecordSignature stamps the index with the embedder and schema it was built
// with. A full rebuild that skips this leaves the next start believing the
// index was built by something else, and rebuilding it again.
func (ix *Index) RecordSignature() error {
	return ix.setMeta("index_signature", ix.Signature())
}

// indexed is what the index already knows about a note.
type indexed struct {
	mtime float64
	hash  string
}

// Sync brings the index in line with the vault, touching only what changed.
func (ix *Index) Sync() (SyncStats, error) {
	var stats SyncStats

	want := ix.Signature()
	have, err := ix.meta("index_signature")
	if err != nil {
		return stats, err
	}
	empty, err := ix.DB.Count("SELECT count(*) FROM notes")
	if err != nil {
		return stats, err
	}
	if have != want || empty == 0 {
		n, err := ix.Reindex()
		if err != nil {
			return stats, err
		}
		if err := ix.setMeta("index_signature", want); err != nil {
			return stats, err
		}
		return SyncStats{Added: n, FullRebuild: true}, nil
	}

	known, err := ix.indexedNotes()
	if err != nil {
		return stats, err
	}
	rels, err := ix.Vault.Walk()
	if err != nil {
		return stats, err
	}

	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()
	// A sync may touch nothing or everything; either way one rebuild at the
	// end beats patching per note.
	defer ix.endBulk()
	ix.beginBulk()

	changed := false
	for _, rel := range rels {
		prev, seen := known[rel]
		delete(known, rel) // whatever remains was deleted from the vault
		if seen {
			// The cheap check first: an untouched file is not opened at all.
			if mtime, _, err := ix.Vault.Stat(rel); err == nil && sameMTime(mtime, prev.mtime) {
				stats.Unchanged++
				continue
			}
		}
		note, err := ix.Vault.Read(rel)
		if err != nil {
			continue // an unreadable file must not abort the sync
		}
		if seen && note.Hash == prev.hash {
			// Touched but not edited — a copy, a sync client rewriting the
			// file, a `touch`. Record the new mtime so the next start takes
			// the cheap path, and re-embed nothing.
			if err := ix.DB.Exec("UPDATE notes SET mtime=? WHERE path=?", note.MTime, rel); err != nil {
				return stats, err
			}
			stats.Unchanged++
			continue
		}
		if err := ix.writeNoteRows(note); err != nil {
			return stats, err
		}
		changed = true
		if seen {
			stats.Updated++
		} else {
			stats.Added++
		}
	}

	for rel := range known {
		if err := ix.removeRows(rel); err != nil {
			return stats, err
		}
		changed = true
		stats.Removed++
	}

	if changed {
		ix.bumpRev()
		// A sync can touch any number of notes, so the whole-vault pass is the
		// right one here — and it is once per sync, not once per note.
		ix.invalidateResolver()
		if err := ix.resolveAll(); err != nil {
			return stats, err
		}
	}
	return stats, nil
}

// sameMTime compares filesystem timestamps with a tolerance, because the mtime
// stored in SQLite is a float64 and filesystems differ in the precision they
// keep. A millisecond is far finer than any edit and far coarser than the
// rounding.
func sameMTime(a, b float64) bool { return math.Abs(a-b) < 0.001 }

func (ix *Index) indexedNotes() (map[string]indexed, error) {
	rows, err := ix.DB.Query("SELECT path, mtime, hash FROM notes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]indexed{}
	for rows.Next() {
		var path, hash string
		var mtime float64
		if err := rows.Scan(&path, &mtime, &hash); err != nil {
			return nil, err
		}
		if !vault.IsReserved(path) {
			out[path] = indexed{mtime: mtime, hash: hash}
		}
	}
	return out, rows.Err()
}

func (ix *Index) meta(key string) (string, error) {
	var v string
	err := ix.DB.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&v)
	if err != nil && err.Error() == "sql: no rows in result set" {
		return "", nil
	}
	return v, err
}

func (ix *Index) setMeta(key, value string) error {
	return ix.DB.Exec("INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)", key, value)
}

// RestampSpaces recomputes which space every row belongs to.
//
// Space membership is a property of a note's path, so changing the set of
// spaces changes it for notes that were indexed long ago. Re-reading and
// re-embedding the vault would cost minutes and produce identical vectors;
// only the space column moves, so only it is rewritten.
func (ix *Index) RestampSpaces(spaceOf func(path string) string) error {
	ix.writeMu.Lock()
	defer ix.writeMu.Unlock()

	rows, err := ix.DB.Query("SELECT path, space FROM notes")
	if err != nil {
		return err
	}
	type change struct{ path, space string }
	var changes []change
	for rows.Next() {
		var path, space string
		if err := rows.Scan(&path, &space); err != nil {
			rows.Close()
			return err
		}
		if want := spaceOf(path); want != space {
			changes = append(changes, change{path, want})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if len(changes) == 0 {
		return nil
	}
	// One transaction for the batch. Each Exec is its own transaction
	// otherwise, which on a 50,000-note vault means 100,000 fsyncs to relabel
	// rows whose contents do not change at all.
	if err := ix.DB.Exec("BEGIN"); err != nil {
		return err
	}
	for _, c := range changes {
		if err := ix.DB.Exec("UPDATE notes SET space=? WHERE path=?", c.space, c.path); err != nil {
			_ = ix.DB.Exec("ROLLBACK")
			return err
		}
		if err := ix.DB.Exec("UPDATE vectors SET space=? WHERE note=?", c.space, c.path); err != nil {
			_ = ix.DB.Exec("ROLLBACK")
			return err
		}
	}
	if err := ix.DB.Exec("COMMIT"); err != nil {
		return err
	}
	// The cache holds each row's space; rather than patch every changed note,
	// drop it once — a space change is rare and touches many notes at a time.
	ix.InvalidateCache()
	ix.bumpRev()
	return nil
}
