// Package version holds the build version of railway-manager.
package version

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

//go:embed VERSION
var embedded string

// Version, Commit and BuildDate are set with -ldflags -X at build time.
// Without ldflags (go install, go run) Version is "v" + the embedded VERSION
// file and Commit/BuildDate come from the VCS build info; failing those they
// are "v0.0.0-dev" and "unknown".
var (
	Version   = ""
	Commit    = ""
	BuildDate = ""
)

func init() {
	var settings map[string]string
	if info, ok := debug.ReadBuildInfo(); ok {
		settings = make(map[string]string, len(info.Settings))
		for _, s := range info.Settings {
			settings[s.Key] = s.Value
		}
	}
	Version, Commit, BuildDate = resolve(Version, Commit, BuildDate, embedded, settings)
}

// resolve fills what ldflags left empty from the VERSION file and the VCS
// build settings, then from fixed fallbacks.
func resolve(v, commit, date, file string, settings map[string]string) (string, string, string) {
	if v == "" {
		if f := strings.TrimSpace(file); f != "" {
			v = "v" + f
		}
	}
	if commit == "" {
		if r := settings["vcs.revision"]; len(r) >= 7 {
			commit = r[:7]
		}
	}
	if date == "" {
		// vcs.time is RFC 3339; keep the date, as the Makefile does.
		date = settings["vcs.time"]
		if len(date) > len("2006-01-02") {
			date = date[:len("2006-01-02")]
		}
	}
	if v == "" {
		v = "v0.0.0-dev"
	}
	if commit == "" {
		commit = "unknown"
	}
	if date == "" {
		date = "unknown"
	}
	return v, commit, date
}
