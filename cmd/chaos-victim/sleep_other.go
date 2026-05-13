//go:build !linux

package main

import "time"

// sleepUntil falls back to time.Sleep on non-Linux platforms. The
// resulting jitter measurements are not reliable — they're useful for
// development on darwin but not for production runs.
func sleepUntil(target time.Time) error {
	d := time.Until(target)
	if d > 0 {
		time.Sleep(d)
	}
	return nil
}

func pinSelf(cpuSet string) error {
	// No-op on non-Linux. The harness only targets RHEL in production.
	return nil
}
