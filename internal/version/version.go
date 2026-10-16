// Package version carries build metadata that is injected at link time via
// -ldflags "-X github.com/vistart/rc_vistart/internal/version.Version=..." .
package version

// These values are overridden at build time by the Makefile.
var (
	// Version is the semantic version of the build.
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build date of the binary.
	Date = "unknown"
)

// String renders the build metadata as "version (commit, date)".
func String() string {
	return Version + " (" + Commit + ", " + Date + ")"
}
