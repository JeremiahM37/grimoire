// Package build reports which build of grimoire is running.
//
// A downloaded binary has to be able to say what it is. Without that, a bug
// report is "the latest one, I think", an operator cannot tell a deployed
// server from the tag they meant to deploy, and a release is unfalsifiable.
package build

import (
	"runtime/debug"
	"strings"
)

// Version is stamped at link time by the release build:
//
//	-ldflags "-X github.com/JeremiahM37/grimoire/go/internal/build.Version=v2.0.0"
var Version = ""

// String is the version, or the best answer available when nothing was
// stamped: `go build` records the commit and whether the tree was dirty, so a
// local build identifies itself honestly rather than claiming a release.
func String() string {
	if Version != "" {
		return Version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "dev"
	}
	var revision, modified string
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			if setting.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		// `go run`, or a build from a tarball with no VCS data.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return "dev"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	return "dev-" + revision + modified
}

// UserAgent identifies grimoire to the services it calls out to.
func UserAgent() string {
	return "grimoire/" + strings.TrimPrefix(String(), "v")
}
