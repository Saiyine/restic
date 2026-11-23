//go:build linux

package main

import (
	"syscall"
)

// peakRSSKiB returns the peak resident set size of the current process in
// KiB.
func peakRSSKiB() (int64, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return int64(ru.Maxrss), true
}
