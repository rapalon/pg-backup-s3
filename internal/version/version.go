// Package version holds build information injected at link time.
//
// The values are set with -ldflags, for example:
//
//	-X pgbackup/internal/version.Version=0.1.0
//	-X pgbackup/internal/version.Commit=abc1234
//	-X pgbackup/internal/version.Date=2026-10-22T00:00:00Z
package version

import (
	"fmt"
	"runtime"
)

// Version is the semantic version of the build ("dev" when unset).
var Version = "dev"

// Commit is the VCS commit the binary was built from.
var Commit = "unknown"

// Date is the build timestamp in RFC 3339.
var Date = "unknown"

// String returns a one-line human-readable description of the build.
func String() string {
	return fmt.Sprintf("pgbackup %s (commit %s, built %s, %s, %s/%s)",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
