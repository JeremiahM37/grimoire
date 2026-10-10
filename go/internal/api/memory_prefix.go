package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/bank"
	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// GET /api/memory/prefix — the durable memories as an append-only log whose
// leading bytes do not move.
//
// Prompt caches (Anthropic's cache_control breakpoints, OpenAI's automatic
// prefix cache) only hit when the bytes at the start of a prompt are identical
// to the bytes at the start of the previous one. /api/memory/profile is built
// for reading, not for caching: its selection is re-ranked on every write and
// its sections are regrouped, so a single new fact can move lines that were
// near the top. This endpoint is built for caching:
//
//   - the membership is the durable set: live (not superseded or expired),
//     not disputed, not pulled from outside, not private, and either written
//     by a person or rated importance 3 or more.
//   - the order is canonical and never re-ranked: first-write stamp, then id.
//     A new fact is written later than every fact before it, so it lands at the
//     end. Recall scores, importance changes and access counts do not reorder
//     anything.
//   - every line has one fixed shape: "- [mem:ID] STAMP TEXT". No counts, no
//     timestamps of the request, no scores appear anywhere in the block, so
//     the only way its bytes change is that the memory changed.
//   - the budget truncates from the END only. Once the block is full, new facts
//     fall off the tail and the head is byte-identical to the last call.
//
// The log is chained: each line's token is a hash over the previous token and
// the line. A caller that remembers the last token it saw can ask
// since=<token> and learn one of three things:
//
//   - unchanged: nothing new is visible under the budget (empty delta);
//   - appended: the earlier entries are exactly what it holds, and delta is
//     only the lines after them;
//   - rewritten: some earlier line changed, was removed (supersession,
//     forgetting, expiry) or was never part of this log. The full block comes
//     back and the caller must replace what it cached.
//
// Volatile, query-dependent recall does not belong in this block. Put it after
// the prefix, in the user turn or in a later system block (see docs/PROMPT-CACHE.md).

const (
	prefixDefaultTokens = 2000
	prefixMinTokens     = 50
	prefixMaxTokens     = 20000
	// prefixMinImportance is the effective importance a non-human fact needs to
	// be in the log. An unrated agent fact ranks as 3, so it qualifies; a fact
	// the writer marked 1 or 2 does not.
	prefixMinImportance = 3
	// prefixHeader is fixed text: it must not carry a count or a date, because
	// anything in it moves on every write and would sit in front of the cache.
	prefixHeader = "# Durable memory (append-only log)\n"
	// prefixGenesis is the token before the first line.
	prefixGenesis = "grimoire-prefix-v1"
)

// prefixEntry is one qualifying fact, after selection and before rendering.
type prefixEntry struct {
	ID    string
	Stamp string
	Text  string
}

// prefixLog is the complete, budget-independent log plus the budgeted view.
type prefixLog struct {
	// Entries and Lines cover every qualifying fact in canonical order.
	Entries []prefixEntry
	Lines   []string
	// Chain[i] is the token after line i; Chain has len(Lines) values.
	Chain []string
	// View is how many leading lines fit the budget.
	View      int
	MaxTokens int
	Block     string
	Tokens    int
	// Version is a hash of Block's bytes: equal bytes, equal version.
	Version string
	// AppendOnlySince is the token at the end of the view. Send it back as
	// since to get only what changed after it.
	AppendOnlySince string
}

// prefixResponse is the JSON the endpoint returns.
type prefixResponse struct {
	Subject         string `json:"subject"`
	Version         string `json:"version"`
	AppendOnlySince string `json:"append_only_since"`
	MaxTokens       int    `json:"max_tokens"`
	Block           string `json:"block,omitempty"`
	Bytes           int    `json:"bytes"`
	Tokens          int    `json:"tokens"`
	Entries         int    `json:"entries"`
	Total           int    `json:"total"`
	Omitted         int    `json:"omitted"`
	Truncated       bool   `json:"truncated"`
	Since           string `json:"since,omitempty"`
	Unchanged       bool   `json:"unchanged,omitempty"`
	Rewritten       bool   `json:"rewritten,omitempty"`
	Appended        int    `json:"appended,omitempty"`
	Delta           string `json:"delta,omitempty"`
	DeltaBytes      int    `json:"delta_bytes,omitempty"`
	DeltaTokens     int    `json:"delta_tokens,omitempty"`
}

// prefixLineFor renders one entry. The format is fixed on purpose.
func prefixLineFor(e prefixEntry) string {
	stamp := e.Stamp
	if stamp == "" {
		stamp = "undated"
	}
	return "- [mem:" + e.ID + "] " + stamp + " " + oneLineText(e.Text) + "\n"
}

// prefixChainStep is the token after line, given the token before it.
func prefixChainStep(prev, line string) string {
	sum := sha256.Sum256([]byte(prev + "\x00" + line))
	return hex.EncodeToString(sum[:16])
}

// selectPrefix returns the qualifying facts in canonical order. Input order
// does not matter: the sort is total because ids are unique.
func selectPrefix(hits []index.MemoryHit, now time.Time) []prefixEntry {
	var out []prefixEntry
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.ID] {
			continue
		}
		if h.Superseded() || h.ExpiredAt(now) || h.Challenges != "" || h.Untrusted() {
			continue
		}
		if !h.HumanAuthored() && h.EffectiveImportance() < prefixMinImportance {
			continue
		}
		seen[h.ID] = true
		out = append(out, prefixEntry{ID: h.ID, Stamp: h.Stamp, Text: h.Text})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Stamp != out[j].Stamp {
			return out[i].Stamp < out[j].Stamp
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// buildPrefix renders the log and cuts the view to maxTokens from the end.
func buildPrefix(hits []index.MemoryHit, now time.Time, maxTokens int) prefixLog {
	l := prefixLog{MaxTokens: maxTokens}
	l.Entries = selectPrefix(hits, now)
	prev := prefixGenesis
	for _, e := range l.Entries {
		line := prefixLineFor(e)
		prev = prefixChainStep(prev, line)
		l.Lines = append(l.Lines, line)
		l.Chain = append(l.Chain, prev)
	}

	// Budget. The per-line costs are summed first, because re-counting the
	// whole block for every candidate line is quadratic. The final block is
	// then counted for real, and the tail is dropped until it fits, so the
	// budget holds on the counter the rest of the engine uses.
	used := bank.CountTokens(prefixHeader)
	view := 0
	for i, line := range l.Lines {
		cost := bank.CountTokens(line)
		if used+cost > maxTokens {
			break
		}
		used += cost
		view = i + 1
	}
	for {
		l.View = view
		l.Block = prefixHeader + strings.Join(l.Lines[:view], "")
		l.Tokens = bank.CountTokens(l.Block)
		if l.Tokens <= maxTokens || view == 0 {
			break
		}
		view--
	}
	sum := sha256.Sum256([]byte(l.Block))
	l.Version = hex.EncodeToString(sum[:16])
	l.AppendOnlySince = l.tokenAt(l.View)
	return l
}

// Omitted is how many qualifying facts fell off the tail to the budget.
func (l prefixLog) Omitted() int { return len(l.Entries) - l.View }

// tokenAt is the chain token after the first n lines.
func (l prefixLog) tokenAt(n int) string {
	if n == 0 {
		return prefixGenesis
	}
	return l.Chain[n-1]
}

// respond answers a request that may carry a since token.
func (l prefixLog) respond(subject, since string) prefixResponse {
	resp := prefixResponse{
		Subject:         subject,
		Version:         l.Version,
		AppendOnlySince: l.AppendOnlySince,
		MaxTokens:       l.MaxTokens,
		Bytes:           len(l.Block),
		Tokens:          l.Tokens,
		Entries:         l.View,
		Total:           len(l.Entries),
		Omitted:         l.Omitted(),
		Truncated:       l.View < len(l.Entries),
	}
	if since == "" {
		resp.Block = l.Block
		return resp
	}
	resp.Since = since
	if since == l.Version {
		resp.Unchanged = true
		return resp
	}
	// Position of the caller's token in the current chain: -1 is the empty
	// log they may have seen. Not found means the caller's log is not a prefix
	// of this one.
	pos := -2
	if since == prefixGenesis {
		pos = -1
	} else {
		for i, tok := range l.Chain {
			if tok == since {
				pos = i
				break
			}
		}
	}
	if pos == -2 {
		resp.Rewritten = true
		resp.Block = l.Block
		return resp
	}
	// Lines the caller holds are pos+1 long. Anything from pos+1 up to the
	// view is new to them; if the view ends at or before that, nothing visible
	// changed.
	if pos+1 >= l.View {
		resp.Unchanged = true
		return resp
	}
	delta := strings.Join(l.Lines[pos+1:l.View], "")
	resp.Delta = delta
	resp.DeltaBytes = len(delta)
	resp.DeltaTokens = bank.CountTokens(delta)
	resp.Appended = l.View - (pos + 1)
	return resp
}

// memoryPrefix is GET /api/memory/prefix?since=TOKEN&max_tokens=N.
func (s *Server) memoryPrefix(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	budget := prefixDefaultTokens
	if raw := strings.TrimSpace(q.Get("max_tokens")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < prefixMinTokens || n > prefixMaxTokens {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("max_tokens must be an integer in %d..%d",
				prefixMinTokens, prefixMaxTokens))
			return
		}
		budget = n
	}
	since := strings.TrimSpace(q.Get("since"))

	now := vault.Now()
	// Private facts never enter a block that is cached and shared across
	// sessions, so the filter is fixed to exclude them regardless of caller.
	f := filterFor(r, false)
	hits, err := s.Index.MemoryEntries(index.MemoryQuery{
		Filter:            f,
		IncludeSuperseded: false,
		IncludeExpired:    false,
		Now:               now,
		Limit:             index.DefaultScanLimit,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	l := buildPrefix(hits, now, budget)
	writeJSON(w, http.StatusOK, l.respond("user", since))
}
