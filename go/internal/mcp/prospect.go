package mcp

import "github.com/JeremiahM37/grimoire/go/internal/exp"

// Intentions (internal/prospect) are behind the `prospect` experiment. With the
// flag off the remember tool's schema and its arguments are exactly what they
// were: the two fields below appear only when the experiment is on.

// withIntention adds the intention fields to a remember schema when the
// experiment is on, and returns the schema unchanged otherwise.
func withIntention(props map[string]any) map[string]any {
	if !exp.On("prospect") {
		return props
	}
	props["cue"] = strProp("makes this an INTENTION: a reminder that fires only when the cue holds. " +
		"Terms joined by &: path:<glob> (deploy/**), cmd:<prefix> (a shell command starts with it), " +
		"project:<name>, after:<YYYY-MM-DD>, before:<YYYY-MM-DD>, term:<word>. " +
		"Example: path:deploy/**&term:staging. Use `cues` for ordinary retrieval hints instead")
	props["life"] = strProp("for an intention: once (retires after its firing is acknowledged), " +
		"standing (the default), or until:<YYYY-MM-DD>")
	return props
}

// intentionArgs lists the argument names the remember tool passes through.
func intentionArgs() []string {
	if !exp.On("prospect") {
		return nil
	}
	return []string{"cue", "life"}
}
