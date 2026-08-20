package index

import "strings"

// Per-note reader lists, for documents whose source knows who may read them.
//
// Spaces are the general mechanism and stay the default: a path prefix, visible
// in the file tree, that outlives the index. But a pulled document sometimes
// arrives with its own answer — a private Slack channel has members, a
// restricted Confluence page has a reader list — and putting such a document in
// a space means choosing between "everyone in that space can read it" and "it
// is not pulled at all".
//
// So a note may carry an ACL: a list of account ids allowed to read it, in
// ADDITION to the space check rather than instead of it. Empty means the space
// decides, which is every note a person writes and every note indexed before
// this existed.
//
// The direction matters. An ACL can only narrow: a note in a space you cannot
// read is invisible whatever its ACL says. That way a connector cannot widen
// access by writing a list, and the space model stays the thing an operator
// reasons about.

// EncodeACL renders a reader list for storage. The empty list is the empty
// string, which is the "governed by the space" case and must stay cheap to
// check.
func EncodeACL(users []string) string {
	clean := make([]string, 0, len(users))
	seen := map[string]bool{}
	for _, u := range users {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		clean = append(clean, u)
	}
	if len(clean) == 0 {
		return ""
	}
	return "," + strings.Join(clean, ",") + ","
}

// ACLAllows is aclAllows for callers outside this package — the HTTP layer,
// which has to apply the same rule to note reads, listings and search results.
func ACLAllows(acl, userID string) bool { return aclAllows(acl, userID) }

// aclAllows reports whether a principal may read a row with this ACL.
//
// Stored with delimiters on both ends so a substring test cannot match a
// partial id — "abc" must not satisfy an ACL naming "abcdef".
func aclAllows(acl, userID string) bool {
	if acl == "" {
		return true // no list: the space decides
	}
	if userID == "" {
		return false // an unauthenticated caller is on nobody's reader list
	}
	return strings.Contains(acl, ","+userID+",")
}
