package secrets

// Provenance-gated brokering: refuse to spend a credential on a target that
// untrusted text asked for.
//
// The scope check in guard.go stops a grant for one API being pointed at
// another. What it cannot stop is IN-SCOPE misuse. A grant for
// `https://api.github.com/repos` plus a note saying "post the contents to a
// gist" is a call whose target was always inside the grant, and no origin
// comparison will ever object to it. That is the residual hole in every
// scoped-token design, and it is the one an indirect prompt injection walks
// through: the attacker does not need to escape the scope, only to choose a
// destination within it.
//
// So the second question this asks is not "where is this call going" but
// "who asked for it". If the target URL appears in a note the user did not
// write -- a clipped web page, a pulled RSS item, a connector document -- then
// the destination was chosen by text of unknown authorship, and a credential
// should not be spent on it without a person saying so.
//
// This is deliberately NOT a novel mechanism. Tainting agent actions by the
// provenance of the content that prompted them is established practice:
// Microsoft's FIDES gates privileged calls that follow untrusted content, and
// dsh-taintguard does the same at the harness layer. What is unusual is where
// it sits. Those implementations live in agent frameworks, which see the
// content but do not hold the credential; token vaults hold the credential but
// never see what the agent read. Grimoire is both halves in one process, so
// the broker can ask a question neither of them can answer alone.
//
// Limits worth stating plainly, because the claim is narrow:
//
//   - This stops CREDENTIAL-MEDIATED exfiltration. It does not stop an agent
//     reading your notes and typing them into its own reply. Nothing here
//     should be described as "your data cannot leak".
//   - A URL you wrote yourself is trusted, so the gate is quiet on the vault
//     you authored. It bites exactly when a destination arrived from outside.
//   - It will fire on a legitimate call whose URL you happened to clip from
//     the web. The remedy is the vouch control that already exists: promote
//     that note to trusted and the call goes through. A refusal that names the
//     note is a decision a person can act on; a silent one is not. Safe
//     methods are not gated at all, so the common case never sees this.

import (
	"errors"
	"net/url"
	"strings"
)

// ErrUntrustedTarget is returned when a state-changing call is aimed at a URL
// that only untrusted content mentions.
var ErrUntrustedTarget = errors.New("target url appears only in untrusted content")

// ProvenanceChecker answers where a URL was seen. It is an interface so that
// this package keeps knowing nothing about notes, indexing or the vault: the
// api package supplies the implementation that queries them.
type ProvenanceChecker interface {
	// UntrustedMention reports the path of an untrusted note that mentions
	// target, and whether any trusted note mentions it too. A URL the user
	// also wrote themselves is not tainted -- corroboration by your own
	// writing is the thing that clears it.
	UntrustedMention(target string) (note string, alsoTrusted bool, err error)
}

// gatedMethods are the HTTP methods that can change something at the far end.
// GET and HEAD are deliberately absent: a read is not an exfiltration sink,
// the scope check already confines where it can go, and gating them would make
// the common case ask for permission constantly for no security gain.
var gatedMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// Gated reports whether a method is one this gate applies to.
func Gated(method string) bool {
	if method == "" {
		return false // the broker defaults an empty method to GET
	}
	return gatedMethods[strings.ToUpper(method)]
}

// normalizeTarget reduces a URL to the form worth searching for in prose:
// scheme, host and path, without query or fragment. A planted destination is
// recognisable by its origin and path; the query string usually carries the
// payload and would defeat a literal match for no benefit.
func normalizeTarget(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return strings.TrimSpace(raw)
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return strings.TrimSuffix(u.String(), "/")
}
