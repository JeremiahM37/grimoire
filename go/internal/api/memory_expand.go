package api

import (
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/index"
)

// Recall expansion and graph walk over HTTP.
//
// The default request path is untouched: recall only takes this route when a
// caller asks for expand or hops (or the expansion default is on). Everything
// here shapes the response for those requests; the ranking and the access
// rules live in index.RecallExpanded.

// recallExpandEnv turns expansion on by default when set to "1". A request's
// own expand parameter always wins over it.
const recallExpandEnv = "GRIMOIRE_RECALL_EXPAND"

// recallExpandOptions reads expand and hops. A malformed hops is refused
// rather than ignored, for the same reason as as_of: a graph answer silently
// built at the wrong depth looks right and is not.
func recallExpandOptions(r *http.Request) (index.ExpandOptions, error) {
	var opt index.ExpandOptions
	if raw := strings.TrimSpace(r.URL.Query().Get("expand")); raw != "" {
		opt.Expand = boolParam(r, "expand")
	} else {
		opt.Expand = os.Getenv(recallExpandEnv) == "1"
	}
	if raw := strings.TrimSpace(r.URL.Query().Get("hops")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > 2 {
			return opt, fmt.Errorf("hops must be 0, 1 or 2")
		}
		opt.Hops = n
	}
	return opt, nil
}

// expandedOut is one recalled fact with the expansion provenance added. It
// embeds entryOut, so the fields a default recall returns are the same and
// the extra ones are only present on an expanded response.
type expandedOut struct {
	entryOut
	Variants []string `json:"variants,omitempty"`
	Hop      int      `json:"hop,omitempty"`
	Via      string   `json:"via,omitempty"`
	Connect  string   `json:"connect,omitempty"`
	Fused    *float64 `json:"fused,omitempty"`
}

// recallExpanded answers a recall that asked for expansion or the graph walk.
func (s *Server) recallExpanded(w http.ResponseWriter, r *http.Request,
	mq index.MemoryQuery, opt index.ExpandOptions) {

	explain := boolParam(r, "explain")
	ehits, err := s.Index.RecallExpanded(mq, opt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	hits := make([]index.MemoryHit, len(ehits))
	for i, e := range ehits {
		hits[i] = e.MemoryHit
	}
	out := s.markProcedures(s.assessHits(hits, entriesOut(hits, explain)))
	res := make([]expandedOut, len(out))
	for i, e := range out {
		x := ehits[i]
		res[i] = expandedOut{entryOut: e, Variants: x.Variants, Hop: x.Hop,
			Via: x.Via, Connect: x.Connect}
		if explain && x.Fused != 0 {
			f := x.Fused
			res[i].Fused = &f
		}
	}
	writeJSON(w, http.StatusOK, res)
}
