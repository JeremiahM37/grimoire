package api

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/trust"
)

// GET /api/trust — what is in this vault, and where it came from.
//
// A policy nobody can see the effect of is a policy nobody maintains. After a
// connector has been running for a month the questions are "how much of my
// corpus is other people's text now?" and "which source is it?", and neither
// is answerable by reading frontmatter across a few thousand files.
//
// It is deliberately a COUNT surface, not a listing: the listing already
// exists (search with trusted=0, or the console's badge filter), and a route
// that returned every untrusted note's title would be a second, unfiltered way
// to enumerate content — the exact shape of the sync-manifest hole.

type originCount struct {
	Origin string `json:"origin"`
	Source string `json:"source"`
	Notes  int    `json:"notes"`
}

func (s *Server) trustOverview(w http.ResponseWriter, r *http.Request) {
	// Counted through the same read check every note surface uses, so on a
	// multi-user instance a member is told about the corpus they can actually
	// retrieve from rather than about somebody else's spaces.
	rows, err := s.Index.DB.Query(
		"SELECT path, COALESCE(origin,''), COALESCE(untrusted,0), COALESCE(acl,'') FROM notes")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type row struct{ path, origin, acl string }
	var all []row
	counts := map[string]int{}
	trusted, untrusted := 0, 0
	for rows.Next() {
		var x row
		// untrusted is selected but not read: the verdict is DERIVED from the
		// origin here, by the same function every other surface calls, so a
		// stale column on a note written before a reindex cannot make this
		// count disagree with what retrieval actually does.
		var stored int
		if err := rows.Scan(&x.path, &x.origin, &stored, &x.acl); err != nil {
			rows.Close()
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		all = append(all, x)
	}
	err = rows.Err()
	// Closed HERE, not by defer: canReadNote queries, and on one connection a
	// query issued while this cursor is open waits for the connection the
	// cursor holds. This deadlock has already been paid for once.
	rows.Close()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	for _, x := range all {
		if !s.canReadNote(r, x.path, x.acl) {
			continue
		}
		if trust.FromOrigin(x.origin) == trust.Untrusted {
			untrusted++
			counts[x.origin]++
			continue
		}
		trusted++
	}

	bySource := []originCount{}
	for origin, n := range counts {
		bySource = append(bySource, originCount{
			Origin: origin, Source: trust.Source(origin), Notes: n})
	}
	// Biggest first, then by name, so the answer is stable across calls — a
	// map's order is not, and a console that re-renders a shuffled list on
	// every poll looks broken.
	sort.Slice(bySource, func(i, j int) bool {
		if bySource[i].Notes != bySource[j].Notes {
			return bySource[i].Notes > bySource[j].Notes
		}
		return bySource[i].Origin < bySource[j].Origin
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"trusted":   trusted,
		"untrusted": untrusted,
		"origins":   bySource,
		// Whether the filter is even worth offering in a UI. On a vault with
		// no connectors the honest answer is "there is nothing to filter".
		"enabled": untrusted > 0,
	})
}

// POST /api/trust/vouch — a person promoting a pulled note to trusted.
//
// It writes `trust: trusted` into the note's frontmatter rather than into a
// table, because that is the whole design: the decision belongs in the file,
// where it survives a reindex, shows up in a diff, and can be undone with an
// editor. A row in the index would be invisible to everything except this
// route, and gone after a rebuild.
func (s *Server) trustVouch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path  string `json:"path"`
		Trust string `json:"trust"`
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
	level := strings.ToLower(strings.TrimSpace(in.Trust))
	if level == "" {
		level = trust.NameTrusted
	}
	if level != trust.NameTrusted && level != trust.NameUntrusted {
		writeErr(w, http.StatusBadRequest, "trust must be trusted or untrusted")
		return
	}
	// Vouching is a WRITE to the note, so it takes the note's write check —
	// not a read check, and not the admin token. Whoever may edit the file may
	// say they have read it.
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
	fm.Set("trust", level)
	if _, err := s.Vault.Write(rel, note.Body, fm); err != nil {
		writeErr(w, statusForVaultErr(err), err.Error())
		return
	}
	updated, err := s.Index.Upsert(rel)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path": rel, "trust": updated.Trust.String(), "origin": updated.Origin})
}
