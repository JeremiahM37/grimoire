package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/JeremiahM37/grimoire/go/internal/index"
	"github.com/JeremiahM37/grimoire/go/internal/vault"
)

// GET /api/stale — the notes most worth re-checking, worst first.
//
// A staleness flag on a retrieval hit tells an agent something about ONE
// passage, which is useful and is not a plan. The plan is a queue: given a
// vault of two thousand notes, which twenty should a person actually go and
// look at this month? That question needs a ranking over the whole corpus, and
// a ranking needs a weight — how much does the vault lean on this note? —
// which is index.StalenessScore. See freshness.go for why the weight is
// inbound links rather than retrieval frequency.

type staleNote struct {
	Path     string  `json:"path"`
	Title    string  `json:"title"`
	AgeDays  int     `json:"age_days"`
	Verified bool    `json:"verified"`
	Inbound  int     `json:"inbound"`
	Score    float64 `json:"score"`
	Origin   string  `json:"origin,omitempty"`
}

func (s *Server) staleNotes(w http.ResponseWriter, r *http.Request) {
	now := vault.Now()
	threshold := index.StaleAfter()
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeErr(w, http.StatusBadRequest, "days must be a non-negative number")
			return
		}
		threshold = time.Duration(n) * 24 * time.Hour
	}
	limit := clampLimit(r.URL.Query().Get("limit"), 20, 200)
	minDays := int(threshold.Hours() / 24)

	// Inbound links, counted once for the whole vault rather than per
	// candidate. A per-note COUNT(*) would be one query per row on a single
	// SQLite connection — the shape that has already produced a hung endpoint
	// in this codebase.
	inbound := map[string]int{}
	linkRows, err := s.Index.DB.Query("SELECT dst FROM links WHERE resolved=1 AND dst IS NOT NULL")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for linkRows.Next() {
		var dst string
		if err := linkRows.Scan(&dst); err != nil {
			linkRows.Close()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		inbound[dst]++
	}
	err = linkRows.Err()
	linkRows.Close()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	type row struct {
		path, title, verified, acl, origin string
		mtime                              float64
	}
	var all []row
	// Private notes are INCLUDED. `private` excludes a note from retrieval by
	// default; it is not an access boundary — the note list already shows
	// private notes to whoever may see the space. Excluding them here would
	// hide exactly the runbooks most worth re-checking from the person who
	// owns them, which is the opposite of what a review queue is for. Access
	// is decided by canReadNote below, the same as every other listing.
	rows, err := s.Index.DB.Query(
		"SELECT path, title, COALESCE(verified,''), COALESCE(acl,''), " +
			"COALESCE(origin,''), mtime FROM notes")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.path, &x.title, &x.verified, &x.acl, &x.origin, &x.mtime); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		all = append(all, x)
	}
	err = rows.Err()
	// Closed before canReadNote, which queries. See the same note in
	// trustOverview and in the briefing.
	rows.Close()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	out := []staleNote{}
	reviewed := 0
	for _, x := range all {
		if !s.canReadNote(r, x.path, x.acl) {
			continue
		}
		// Memory notes are excluded on purpose. Facts carry their own
		// lifecycle — TTL, recency decay, supersession — so listing a memory
		// note here would ask a person to re-verify something the engine
		// already manages, which is busywork that teaches people to ignore the
		// queue.
		if strings.HasPrefix(x.path, MemoryDir+"/") {
			continue
		}
		age, explicit := index.Freshness(x.verified, x.mtime).AgeDays(now)
		if explicit {
			reviewed++
		}
		if minDays > 0 && age < minDays {
			continue
		}
		n := inbound[x.path]
		out = append(out, staleNote{
			Path: x.path, Title: x.title, AgeDays: age, Verified: explicit,
			Inbound: n, Score: index.StalenessScore(age, n), Origin: x.origin,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Path < out[j].Path
	})
	total := len(out)
	if len(out) > limit {
		out = out[:limit]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"notes": out,
		// total, not len(notes): a queue that says "20" when there are 340 is
		// how a backlog stays invisible.
		"total":     total,
		"threshold": minDays,
		// How many notes anybody has ever explicitly confirmed. On a vault
		// where nobody uses `verified:` the queue is really an age listing,
		// and saying so is more honest than letting it look like a review
		// process that is running.
		"reviewed": reviewed,
	})
}

// POST /api/stale/verify — "I have read this and it is still true."
//
// Writes a `verified:` date into the note's frontmatter, for the same reason
// vouching writes `trust:` there: the claim belongs in the file, where it
// survives a reindex, shows up in a diff, and is undone with an editor.
func (s *Server) verifyNote(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path string `json:"path"`
		Date string `json:"date"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	rel := normPath(in.Path)
	if rel == "" {
		writeErr(w, http.StatusBadRequest, "path required")
		return
	}
	stamp := strings.TrimSpace(in.Date)
	if stamp == "" {
		stamp = vault.Now().Format("2006-01-02")
	} else if !index.ValidVerifiedDate(stamp) {
		// A date the parser will not read would silently do nothing: the note
		// would carry a `verified:` line and still be reported as never
		// checked, which is the most confusing possible outcome.
		writeErr(w, http.StatusBadRequest,
			"date must be YYYY-MM-DD, YYYY-MM-DD HH:MM or RFC3339")
		return
	}
	if !s.requireWrite(w, r, rel) {
		return
	}
	note, err := s.Vault.Read(rel)
	if err != nil {
		writeErr(w, http.StatusNotFound, "note not found")
		return
	}
	s.History.Snapshot(rel, note.Body)
	fm := note.Frontmatter.Clone()
	fm.Set("verified", stamp)
	if _, err := s.Vault.Write(rel, note.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	if _, err := s.Index.Upsert(rel); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "verified": stamp})
}
