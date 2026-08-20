package auth

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// Spaces: what an identity may read.
//
// A space is a subtree of the vault plus the people who may see it. Mapping
// access to a path prefix rather than to a per-note access list is the choice
// this file is built on, and it follows from what Grimoire is: plain markdown
// files that outlive the app. A prefix is visible in the file tree, survives
// being copied to another machine, and can be reasoned about without the
// database — while a per-note ACL exists only in an index that is meant to be
// disposable, and would silently open up every note if it were ever rebuilt
// from files alone.
//
// Three kinds:
//
//	personal — one per account, prefix users/<name>/, member: its owner
//	shared   — created by an administrator, prefix chosen by them
//	commons  — the implicit space every other path belongs to
//
// The commons is what keeps a single-user vault working unchanged: with no
// accounts every note is in it and everything is visible. With accounts, the
// commons is readable by everyone and writable by anyone whose role allows it,
// which is the behaviour a shared team vault wants for its root.

// Space kinds.
const (
	KindPersonal = "personal"
	KindShared   = "shared"
	KindCommons  = "commons"
)

// Membership roles within a space.
const (
	SpaceReader = "reader"
	SpaceWriter = "writer"
)

// CommonsID is the implicit space holding everything outside any prefix.
const CommonsID = "commons"

// PersonalPrefix is where personal spaces live.
const PersonalPrefix = "users/"

var (
	ErrNoSuchSpace = errors.New("no such space")
	ErrBadPrefix   = errors.New("a space prefix must be a non-empty path with no . or .. segments")
	ErrPrefixTaken = errors.New("a space already covers that prefix")
)

// Space is one access boundary.
type Space struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Prefix  string `json:"prefix"`
	Kind    string `json:"kind"`
	Owner   string `json:"owner,omitempty"`
	Created string `json:"created"`
}

// Member is an account's membership of a space.
type Member struct {
	Space string `json:"space"`
	User  string `json:"user"`
	Name  string `json:"name"`
	Role  string `json:"role"`
}

// CreateSpace adds a shared space.
func (s *Store) CreateSpace(name, prefix, kind, owner string) (Space, error) {
	prefix = NormalizePrefix(prefix)
	if prefix == "" {
		return Space{}, ErrBadPrefix
	}
	taken, err := s.DB.Count("SELECT count(*) FROM spaces WHERE prefix=?", prefix)
	if err != nil {
		return Space{}, err
	}
	if taken > 0 {
		return Space{}, ErrPrefixTaken
	}
	if kind != KindPersonal {
		kind = KindShared
	}
	sp := Space{ID: newID(), Name: strings.TrimSpace(name), Prefix: prefix, Kind: kind,
		Owner: owner, Created: Now().UTC().Format(time.RFC3339)}
	if sp.Name == "" {
		sp.Name = strings.TrimSuffix(prefix, "/")
	}
	if err := s.DB.Exec(
		"INSERT INTO spaces(id,name,prefix,kind,owner,created) VALUES(?,?,?,?,?,?)",
		sp.ID, sp.Name, sp.Prefix, sp.Kind, sp.Owner, sp.Created); err != nil {
		return Space{}, err
	}
	if owner != "" {
		if err := s.AddMember(sp.ID, owner, SpaceWriter); err != nil {
			return Space{}, err
		}
	}
	return sp, nil
}

// EnsurePersonalSpace creates an account's own space if it does not exist.
func (s *Store) EnsurePersonalSpace(u User) (Space, error) {
	prefix := PersonalPrefix + u.Name + "/"
	if sp, err := s.SpaceByPrefix(prefix); err == nil {
		return sp, nil
	}
	return s.CreateSpace(u.Display+"'s notes", prefix, KindPersonal, u.ID)
}

// NormalizePrefix cleans a prefix to the "dir/" form paths are matched against.
func NormalizePrefix(prefix string) string {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix == "" {
		return ""
	}
	for _, seg := range strings.Split(prefix, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
	}
	return prefix + "/"
}

// Spaces lists every space, longest prefix first — the order paths are
// matched in, so a nested space wins over the one containing it.
func (s *Store) Spaces() ([]Space, error) {
	rows, err := s.DB.Query("SELECT id,name,prefix,kind,owner,created FROM spaces")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Space
	for rows.Next() {
		var sp Space
		if err := rows.Scan(&sp.ID, &sp.Name, &sp.Prefix, &sp.Kind, &sp.Owner, &sp.Created); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].Prefix) != len(out[j].Prefix) {
			return len(out[i].Prefix) > len(out[j].Prefix)
		}
		return out[i].Prefix < out[j].Prefix
	})
	return out, nil
}

// SpaceByPrefix finds a space by its exact prefix.
func (s *Store) SpaceByPrefix(prefix string) (Space, error) {
	var sp Space
	err := s.DB.QueryRow(
		"SELECT id,name,prefix,kind,owner,created FROM spaces WHERE prefix=?",
		NormalizePrefix(prefix)).Scan(&sp.ID, &sp.Name, &sp.Prefix, &sp.Kind, &sp.Owner, &sp.Created)
	if err != nil {
		return Space{}, ErrNoSuchSpace
	}
	return sp, nil
}

// GetSpace finds a space by id.
func (s *Store) GetSpace(id string) (Space, error) {
	if id == CommonsID {
		return Space{ID: CommonsID, Name: "Commons", Prefix: "", Kind: KindCommons}, nil
	}
	var sp Space
	err := s.DB.QueryRow(
		"SELECT id,name,prefix,kind,owner,created FROM spaces WHERE id=?", id).
		Scan(&sp.ID, &sp.Name, &sp.Prefix, &sp.Kind, &sp.Owner, &sp.Created)
	if err != nil {
		return Space{}, ErrNoSuchSpace
	}
	return sp, nil
}

// DeleteSpace removes a space and its memberships. The notes stay on disk and
// fall back to whichever space now covers their path — usually the commons.
func (s *Store) DeleteSpace(id string) error {
	if err := s.DB.Exec("DELETE FROM space_members WHERE space=?", id); err != nil {
		return err
	}
	return s.DB.Exec("DELETE FROM spaces WHERE id=?", id)
}

// AddMember grants an account access to a space.
func (s *Store) AddMember(spaceID, userID, role string) error {
	if role != SpaceReader {
		role = SpaceWriter
	}
	return s.DB.Exec(
		"INSERT OR REPLACE INTO space_members(space,user,role) VALUES(?,?,?)",
		spaceID, userID, role)
}

// RemoveMember revokes access.
func (s *Store) RemoveMember(spaceID, userID string) error {
	return s.DB.Exec("DELETE FROM space_members WHERE space=? AND user=?", spaceID, userID)
}

// Members lists a space's members with their account names.
func (s *Store) Members(spaceID string) ([]Member, error) {
	rows, err := s.DB.Query(
		"SELECT m.space, m.user, u.name, m.role FROM space_members m "+
			"JOIN users u ON u.id=m.user WHERE m.space=? ORDER BY u.name", spaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.Space, &m.User, &m.Name, &m.Role); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SpaceOf returns the id of the space a note path belongs to: the longest
// matching prefix, or the commons.
func SpaceOf(path string, spaces []Space) string {
	path = strings.TrimPrefix(path, "/")
	for _, sp := range spaces { // already longest-first
		if sp.Prefix != "" && strings.HasPrefix(path, sp.Prefix) {
			return sp.ID
		}
	}
	return CommonsID
}

// Principal is an authenticated caller and what it may do.
//
// Single-user deployments produce an unrestricted principal, which is what
// keeps every existing surface working with no accounts configured.
type Principal struct {
	User         User
	Anonymous    bool
	Unrestricted bool // no accounts exist: the historical single-user server
	// readable and writable are space ids; nil in unrestricted mode.
	readable map[string]bool
	writable map[string]bool
}

// Unrestricted is the principal a deployment without accounts serves.
func Unrestricted() *Principal { return &Principal{Unrestricted: true} }

// Anonymous is a caller who has not authenticated on a multi-user deployment.
func Anonymous() *Principal { return &Principal{Anonymous: true} }

// IsAdmin reports administrative rights. An unrestricted principal has them:
// with no accounts there is nobody else to administer, and the deployment's
// own operator is the one holding the port.
func (p *Principal) IsAdmin() bool {
	return p.Unrestricted || (!p.Anonymous && p.User.IsAdmin())
}

// CanRead reports whether the principal may see notes in a space.
func (p *Principal) CanRead(spaceID string) bool {
	if p.Unrestricted {
		return true
	}
	if p.Anonymous {
		return false
	}
	return p.readable[spaceID]
}

// CanWrite reports whether the principal may change notes in a space.
func (p *Principal) CanWrite(spaceID string) bool {
	if p.Unrestricted {
		return true
	}
	if p.Anonymous {
		return false
	}
	return p.writable[spaceID]
}

// ReadableSpaces returns the space ids the principal may read, for the index
// to filter on. A nil result means "everything" (unrestricted).
func (p *Principal) ReadableSpaces() map[string]bool {
	if p.Unrestricted {
		return nil
	}
	if p.readable == nil {
		// An anonymous principal carries no map, and nil means "no
		// restriction" downstream — so returning it directly would hand an
		// unauthenticated caller the whole vault. Empty and unset are
		// different answers and this is where they are told apart.
		return map[string]bool{}
	}
	return p.readable
}

// Name identifies the principal in logs, audit records and agent memory.
func (p *Principal) Name() string {
	switch {
	case p.Unrestricted:
		return "local"
	case p.Anonymous:
		return "anonymous"
	default:
		return p.User.Name
	}
}

// PrincipalFor builds a principal for an account: its personal space, every
// space it is a member of, and the commons.
//
// Administrators can read every space. That is a deliberate simplification of
// what a larger system would do — it means "admin" and "can read everyone's
// notes" are the same privilege — and it is stated here rather than discovered
// later, because the alternative is an administrator who cannot fix a space
// they cannot see.
func (s *Store) PrincipalFor(u User) (*Principal, error) {
	p := &Principal{User: u, readable: map[string]bool{}, writable: map[string]bool{}}
	// Everyone reads and writes the commons: it is the shared root of the
	// vault, and a team that cannot write there has no shared root at all.
	p.readable[CommonsID] = true
	p.writable[CommonsID] = true

	spaces, err := s.Spaces()
	if err != nil {
		return nil, err
	}
	if u.IsAdmin() {
		for _, sp := range spaces {
			p.readable[sp.ID] = true
			p.writable[sp.ID] = true
		}
		return p, nil
	}

	rows, err := s.DB.Query("SELECT space, role FROM space_members WHERE user=?", u.ID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var space, role string
		if err := rows.Scan(&space, &role); err != nil {
			return nil, err
		}
		p.readable[space] = true
		if role == SpaceWriter {
			p.writable[space] = true
		}
	}
	return p, rows.Err()
}
