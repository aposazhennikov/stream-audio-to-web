// Package version holds the build information of the application.
// The values are set at build time with -ldflags, for example:
//
//	go build -ldflags "-X github.com/aposazhennikov/stream-audio-to-web/version.Version=2026.10.29-c9a6b2b"
package version

import (
	"runtime/debug"
	"time"
)

const shortCommitLen = 7

// Set with -ldflags -X at build time.
var (
	Version   = ""
	Commit    = ""
	BuildTime = "" // RFC 3339, UTC.
)

// startTime is when the process started; used to report uptime.
var startTime = time.Now()

// Info describes the running build.
type Info struct {
	Version     string `json:"version"`
	Commit      string `json:"commit"`
	ShortCommit string `json:"shortCommit"`
	BuildTime   string `json:"buildTime"`
	StartedAt   string `json:"startedAt"`
	GoVersion   string `json:"goVersion"`
}

// Get returns the build information. Values missing from -ldflags are taken from
// the VCS data that Go embeds when building inside a git checkout; a local build
// without either reports "dev".
func Get() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		StartedAt: startTime.UTC().Format(time.RFC3339),
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		info.GoVersion = bi.GoVersion
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				if info.Commit == "" {
					info.Commit = s.Value
				}
			case "vcs.time":
				if info.BuildTime == "" {
					info.BuildTime = s.Value
				}
			}
		}
	}

	info.ShortCommit = info.Commit
	if len(info.ShortCommit) > shortCommitLen {
		info.ShortCommit = info.ShortCommit[:shortCommitLen]
	}
	if info.Version == "" {
		info.Version = "dev"
		if info.ShortCommit != "" {
			info.Version = "dev-" + info.ShortCommit
		}
	}
	return info
}
