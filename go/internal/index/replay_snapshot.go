package index

import (
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/memory"
)

// ReplayFact is a stored fact the way context injection can see it, with its
// embedding, for memory replay (docs/MEMORY_REPLAY.md).
type ReplayFact struct {
	ID, Note, Text string
	Vec            []float32
}

// ReplayChunk is one embedded chunk of an ordinary note.
type ReplayChunk struct {
	Note, Text string
	Vec        []float32
}

// ReplayFacts returns the facts injection may show: current, accepted, public
// and trusted. It is the candidate set MemoryEntries builds for the hybrid
// context, with the embeddings kept.
func (ix *Index) ReplayFacts(now time.Time) ([]ReplayFact, error) {
	rows, err := ix.DB.Query(`SELECT id,note,text,expires,origin,embedding FROM memory_entries
		WHERE superseded_by='' AND challenges='' AND private=0 AND visibility='' ORDER BY stamp DESC, id DESC LIMIT ?`, DefaultScanLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplayFact
	for rows.Next() {
		var f ReplayFact
		var e memory.Entry
		var blob []byte
		if err := rows.Scan(&f.ID, &f.Note, &f.Text, &e.Expires, &e.Origin, &blob); err != nil {
			return nil, err
		}
		e.Text = f.Text
		if e.ExpiredAt(now) || e.Untrusted() {
			continue
		}
		f.Vec = Unpack(blob)
		out = append(out, f)
	}
	return out, rows.Err()
}

// ReplayChunks returns the embedded chunks of public, trusted notes outside
// the memory directory, in note and chunk order.
func (ix *Index) ReplayChunks() ([]ReplayChunk, error) {
	return ix.replayChunks(false)
}

// ReplayMemoryChunks returns the chunks of memory notes. Retrieval ranks them
// with everything else before injection drops them, so they take slots in its
// top results; replay needs them to count the same slots.
func (ix *Index) ReplayMemoryChunks() ([]ReplayChunk, error) {
	return ix.replayChunks(true)
}

func (ix *Index) replayChunks(memory bool) ([]ReplayChunk, error) {
	op := "NOT LIKE"
	if memory {
		op = "LIKE"
	}
	rows, err := ix.DB.Query(`SELECT note,chunk,embedding FROM vectors
		WHERE private=0 AND COALESCE(untrusted,0)=0 AND note `+op+` ? ORDER BY note, chunk_idx`, MemoryPrefix+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplayChunk
	for rows.Next() {
		var c ReplayChunk
		var blob []byte
		if err := rows.Scan(&c.Note, &c.Text, &blob); err != nil {
			return nil, err
		}
		c.Vec = Unpack(blob)
		out = append(out, c)
	}
	return out, rows.Err()
}
