// Package version carries the release version of the binary.
package version

import (
	"runtime/debug"
	"strings"
)

// Version is the release version. Release builds stamp the git tag via
// -ldflags -X at link time (see Makefile and .github/workflows/release.yml);
// a plain source build leaves the "dev" placeholder here and init() replaces it
// with whatever the toolchain embedded (see below).
var Version = "dev"

// Commit is the git commit a build was stamped from (short form). Source
// builds keep it empty; release/dev builds stamp it with -ldflags -X. It
// answers "which exact tree is running" for memento_status — version alone
// cannot distinguish two dev builds.
var Commit = ""

// init fills in what a plain `go build` or `go install module@version` knows
// about itself: the module version and the VCS revision the toolchain embedded
// (buildvcs). Without it `go install ...@latest` reported "dev" with an empty
// commit, which contradicts the version/commit fields memento_status promises.
// Values stamped with -ldflags always win.
func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return
	}
	if Version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		Version = strings.TrimPrefix(info.Main.Version, "v")
	}
	if Commit != "" {
		return
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			Commit = s.Value[:7]
			return
		}
	}
}
