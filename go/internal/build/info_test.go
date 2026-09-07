package build

import (
	"runtime/debug"
	"testing"
)

func TestBuildIdentityPreservesDirtyAndUnknown(t *testing.T) {
	for _, dirty := range []string{"true", "false"} {
		got := fromBuildInfo(&debug.BuildInfo{Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1234567890abcdef"},
			{Key: "vcs.modified", Value: dirty},
			{Key: "vcs.time", Value: "2026-09-07T12:00:00Z"},
		}})
		if got.Revision != "1234567890abcdef" || got.Modified == nil || *got.Modified != (dirty == "true") || got.CommitTime == "" {
			t.Fatalf("lost build identity: %+v", got)
		}
	}
	got := fromBuildInfo(nil)
	if got.Revision != "" || got.Modified != nil {
		t.Fatalf("unknown metadata claimed known: %+v", got)
	}
}
