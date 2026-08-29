package auth

import (
	"strings"
)

// Who a person is in a system Grimoire pulls from.
//
// A connector knows a Slack user id or an Atlassian account id. Only this table
// can say which Grimoire account that is, and the mapping has to be explicit:
// matching on email would silently grant access to whoever controls an address
// a source happens to report, and guessing from display names is worse.
//
// An UNMAPPED identity is nobody. That is the important half — a document
// readable by five people in Slack, three of whom have Grimoire accounts, is
// readable by those three and by nobody else. The alternative (unmapped means
// everyone) would turn an incomplete mapping into a leak, silently, at exactly
// the moment someone new joins.

// MapIdentity records that an external identity is this account.
func (s *Store) MapIdentity(source, external, userID string) error {
	source = strings.ToLower(strings.TrimSpace(source))
	external = strings.TrimSpace(external)
	if source == "" || external == "" || userID == "" {
		return ErrInvalidName
	}
	if _, err := s.Get(userID); err != nil {
		return err
	}
	return s.DB.Exec(
		"INSERT OR REPLACE INTO identities(source, external, user) VALUES(?,?,?)",
		source, external, userID)
}

// UnmapIdentity removes a mapping.
func (s *Store) UnmapIdentity(source, external string) error {
	return s.DB.Exec("DELETE FROM identities WHERE source=? AND external=?",
		strings.ToLower(strings.TrimSpace(source)), strings.TrimSpace(external))
}

// Identity is one mapping, for listing.
type Identity struct {
	Source   string `json:"source"`
	External string `json:"external"`
	User     string `json:"user"`
	Name     string `json:"name"`
}

// Identities lists every mapping, with account names.
func (s *Store) Identities() ([]Identity, error) {
	rows, err := s.DB.Query(
		"SELECT i.source, i.external, i.user, u.name FROM identities i " +
			"JOIN users u ON u.id=i.user ORDER BY i.source, u.name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Identity
	for rows.Next() {
		var i Identity
		if err := rows.Scan(&i.Source, &i.External, &i.User, &i.Name); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ResolveIdentities turns a source's reader list into Grimoire account ids.
//
// Identities with no mapping are dropped, deliberately: see the package note.
// The count of dropped ones is returned so a caller can report "this document
// is readable by four people in Slack, two of whom have accounts here", which
// is the sentence an operator needs to see before trusting the result.
func (s *Store) ResolveIdentities(source string, external []string) (users []string, unmapped int, err error) {
	if len(external) == 0 {
		return nil, 0, nil
	}
	source = strings.ToLower(strings.TrimSpace(source))
	seen := map[string]bool{}
	for _, ext := range external {
		ext = strings.TrimSpace(ext)
		if ext == "" {
			continue
		}
		var userID string
		err := s.DB.QueryRow("SELECT user FROM identities WHERE source=? AND external=?",
			source, ext).Scan(&userID)
		if err != nil || userID == "" {
			unmapped++
			continue
		}
		if !seen[userID] {
			seen[userID] = true
			users = append(users, userID)
		}
	}
	return users, unmapped, nil
}

// UserForIdentity resolves one external identity to an account.
//
// Used by the network-identity backends, where the external id is a tailnet
// login or a ZeroTier node. The mapping is explicit for the reason at the top
// of this file: a verified identity says truthfully who is calling, and says
// nothing at all about what they may read. Somebody has to decide that, once,
// on purpose.
func (s *Store) UserForIdentity(source, external string) (User, error) {
	users, _, err := s.ResolveIdentities(source, []string{external})
	if err != nil {
		return User{}, err
	}
	if len(users) == 0 {
		return User{}, ErrNoSuchUser
	}
	return s.Get(users[0])
}
