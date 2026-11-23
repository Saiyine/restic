//go:build !windows

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// setSymlinkModTime sets the modification time of the symlink itself
// without following it.
func setSymlinkModTime(path string, tm time.Time) error {
	ts := []unix.Timespec{{Sec: tm.Unix()}, {Sec: tm.Unix()}}
	return unix.UtimesNanoAt(unix.AT_FDCWD, path, ts, unix.AT_SYMLINK_NOFOLLOW)
}
