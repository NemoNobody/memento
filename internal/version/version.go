// Package version carries the release version of the binary.
package version

// Version is the release version. Source builds keep it "dev"; release
// builds stamp the git tag via -ldflags -X at link time (see Makefile and
// .github/workflows/release.yml).
var Version = "dev"

// Commit is the git commit a build was stamped from (short form). Source
// builds keep it empty; release/dev builds stamp it with -ldflags -X. It
// answers "which exact tree is running" for memento_status — version alone
// cannot distinguish two dev builds.
var Commit = ""
