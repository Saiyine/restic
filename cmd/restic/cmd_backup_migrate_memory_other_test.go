//go:build !linux

package main

// peakRSSKiB is not available on this platform.
func peakRSSKiB() (int64, bool) {
	return 0, false
}
