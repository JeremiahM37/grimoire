package build

import "runtime/debug"

// Info identifies the running executable, independently of its release label.
// A nil Modified value means VCS metadata was not recorded, not a clean build.
type Info struct {
	Version    string `json:"version"`
	Revision   string `json:"revision"`
	Modified   *bool  `json:"modified"`
	CommitTime string `json:"commit_time,omitempty"`
}

func Current() Info {
	info, _ := debug.ReadBuildInfo()
	return fromBuildInfo(info)
}

func fromBuildInfo(info *debug.BuildInfo) Info {
	out := Info{Version: String()}
	if info == nil {
		return out
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			out.Revision = s.Value
		case "vcs.time":
			out.CommitTime = s.Value
		case "vcs.modified":
			if s.Value == "true" || s.Value == "false" {
				dirty := s.Value == "true"
				out.Modified = &dirty
			}
		}
	}
	return out
}
