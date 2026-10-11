package mcp

// remoteCallerArg is the argument key callTool sets on an HTTP call. It is never
// read from the caller's own arguments as a grant: the server writes it, and
// it can only restrict a write.
const remoteCallerArg = "\x00remote_caller"

// rememberAnchors is the anchor part of the remember description. It states
// the contract an agent writes against and must stay under 120 words (see
// TestRememberAnchorsDescriptionIsShort).
const rememberAnchors = "KINDS (docs/MEMORY-ANCHORS.md): an event happened and stays true as " +
	"history (event=true, no anchors); a preference or rule is something the user wants " +
	"(no anchors); a claim says how something IS now and can later go false. A claim " +
	"should carry `anchors` naming the specific thing it is about, not something near it. " +
	"Probe types: file_exists, dir_exists, path_absent, symlink_to, file_contains, " +
	"unit_active, unit_inactive, unit_enabled, port_listening, port_closed, http_status, " +
	"http_contains, binary_version, git_commit, git_branch, remote. Anchors are checked " +
	"when you write; a failing one is reported back and not stored."
