// Package version carries the release version of the binary.
package version

// Version is the release version. Source builds keep it "dev"; release
// builds stamp the git tag via -ldflags -X at link time (see Makefile and
// .github/workflows/release.yml).
var Version = "dev"
