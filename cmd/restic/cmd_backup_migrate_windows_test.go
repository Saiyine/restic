//go:build windows

package main

import (
	"time"
)

// setSymlinkModTime is a no-op on windows; symlinks are not created there.
func setSymlinkModTime(_ string, _ time.Time) error {
	return nil
}
