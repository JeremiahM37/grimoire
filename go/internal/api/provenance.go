package api

// The notes-backed answer to "who chose this destination?".
//
// secrets.Broker refuses a state-changing brokered call whose target URL is
// named only by content the user did not write. That check needs to know what
// the vault says about a URL, which is a question only this package can
// answer, so the broker takes an interface and this supplies it.
//
// See go/internal/secrets/provenance.go for the reasoning and the limits.

import (
	"strings"

	"github.com/JeremiahM37/grimoire/go/internal/db"
)

// notesProvenance looks a URL up in note bodies, split by trust.
type notesProvenance struct{ db *db.DB }

// likeEscape neutralises the LIKE wildcards in a URL before it is used as a
// pattern. A target containing "%" or "_" would otherwise match far more than
// itself — "_" matches any character, so a URL with an underscore in the path
// would silently widen the search and could clear a genuinely planted URL by
// matching some unrelated trusted note.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// UntrustedMention reports an untrusted note that mentions target, and whether
// any trusted note mentions it too.
//
// The trusted lookup is what keeps this quiet in normal use: a URL you wrote
// in your own notes is corroborated by your own writing, and a call to it is
// not the attack this exists to stop. Only a destination that appears solely
// in text of unknown authorship is refused.
func (p notesProvenance) UntrustedMention(target string) (string, bool, error) {
	target = strings.TrimSpace(target)
	// A bare origin is too coarse to be evidence of anything: half the web
	// mentions api.github.com. Requiring a path keeps this to destinations
	// specific enough that naming one is a choice.
	if len(target) < 12 || !strings.Contains(target, "://") {
		return "", false, nil
	}
	if u := strings.SplitN(target, "://", 2)[1]; !strings.Contains(u, "/") {
		return "", false, nil
	}
	pattern := "%" + likeEscape(target) + "%"

	var note string
	err := p.db.QueryRow(
		`SELECT path FROM notes WHERE untrusted=1 AND body LIKE ? ESCAPE '\' LIMIT 1`,
		pattern).Scan(&note)
	if err != nil || note == "" {
		return "", false, nil // nothing untrusted names it: nothing to refuse
	}
	var trusted string
	if err := p.db.QueryRow(
		`SELECT path FROM notes WHERE untrusted=0 AND body LIKE ? ESCAPE '\' LIMIT 1`,
		pattern).Scan(&trusted); err == nil && trusted != "" {
		return note, true, nil
	}
	return note, false, nil
}
