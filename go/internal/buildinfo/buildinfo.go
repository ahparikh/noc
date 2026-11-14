// Package buildinfo carries build metadata, intended to be set with -ldflags -X.
package buildinfo

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String returns Version, or "Version (Commit)" when a commit is known.
func String() string {
	if Commit == "" || Commit == "none" {
		return Version
	}
	return Version + " (" + Commit + ")"
}
