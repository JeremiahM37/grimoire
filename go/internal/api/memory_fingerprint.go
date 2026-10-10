package api

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"sync"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/adherence"
	fpr "github.com/JeremiahM37/grimoire/go/internal/fingerprint"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// fpDFTTL is how long the store-wide document frequencies are reused. They
// move slowly; a stale count only shifts a rarity score a little.
const fpDFTTL = 10 * time.Minute

type fpCache struct {
	mu   sync.Mutex
	df   fpr.DF
	made time.Time
}

// fingerprintDF is the document frequency of every candidate token across the
// whole memory store (all accepted memories, whoever may read them: only
// counts are used, never text), rebuilt at most every fpDFTTL.
func (s *Server) fingerprintDF() fpr.DF {
	c := &s.fpDF
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.df.Count != nil && time.Since(c.made) < fpDFTTL {
		return c.df
	}
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter: index.Filter{IncludePrivate: true, IgnoreACLs: true}, AcceptedOnly: true,
		Limit: 20000, Now: vault.Now()})
	if err != nil {
		if c.df.Count != nil {
			return c.df
		}
		return fpr.DF{Count: map[string]int{}}
	}
	texts := make([]string, len(hits))
	for i, h := range hits {
		texts[i] = h.Text
	}
	c.df, c.made = fpr.BuildDF(texts), time.Now()
	return c.df
}

// itemFingerprints picks the fingerprints of each injected item, leaving out
// whatever the situation (the prompt or command that triggered the injection)
// already contained.
func (s *Server) itemFingerprints(picked []contextItem, situation string) [][]fpr.Fingerprint {
	df := s.fingerprintDF()
	sit := fpr.Set(situation)
	out := make([][]fpr.Fingerprint, len(picked))
	for i, it := range picked {
		out[i] = fpr.Select(it.Text, df, sit, fpr.Options{})
	}
	return out
}

// fingerprintWire is the response section the client hook stores: salted
// hashes per tag, never the tokens. The salt is fresh for every response, so
// a hash seen in one session says nothing about another.
func fingerprintWire(tags []string, fps [][]fpr.Fingerprint) map[string]any {
	var raw [8]byte
	rand.Read(raw[:])
	salt := hex.EncodeToString(raw[:])
	items := map[string][]string{}
	none := []string{}
	for i, f := range fps {
		if len(f) == 0 {
			none = append(none, tags[i])
			continue
		}
		hs := make([]string, len(f))
		for j, x := range f {
			hs[j] = fpr.Hash(salt, x.Token)
		}
		items[tags[i]] = hs
	}
	return map[string]any{"v": fpr.SpecVersion, "spec": fpr.Spec, "salt": salt,
		"items": items, "none": none}
}

// recordFingerprintCounts takes the client's matched counts. It is called
// before a Stop is finalised so the match counts in the same turn, and also
// upgrades a row an earlier Stop already closed as ignored.
func (s *Server) recordFingerprintCounts(r *http.Request, st *adherence.Store, session string, counts map[string]int, since time.Time) int {
	late, err := st.MarkFingerprints(session, counts, since)
	if err != nil {
		return 0
	}
	for _, row := range late {
		s.applyOutcome(r, row)
	}
	return len(counts)
}
