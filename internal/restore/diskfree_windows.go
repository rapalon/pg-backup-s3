//go:build windows

package restore

import "errors"

// diskFree is not implemented on Windows; the binary targets Linux. Tests
// inject Deps.DiskFree.
func diskFree(string) (uint64, error) {
	return 0, errors.New("free space check not supported on windows")
}
