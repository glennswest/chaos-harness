//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// sleepUntilMono blocks until the monotonic-clock deadline targetNs.
// targetNs is nanoseconds since some unspecified reference (CLOCK_MONOTONIC
// epoch — usually boot time), NOT wall-clock UnixNano.
func sleepUntilMono(targetNs int64) error {
	ts := unix.Timespec{
		Sec:  targetNs / 1_000_000_000,
		Nsec: targetNs % 1_000_000_000,
	}
	for {
		err := unix.ClockNanosleep(unix.CLOCK_MONOTONIC, unix.TIMER_ABSTIME, &ts, nil)
		if err == nil {
			return nil
		}
		if err == unix.EINTR {
			continue
		}
		return err
	}
}

// nowMonoNs returns the current CLOCK_MONOTONIC value in nanoseconds.
func nowMonoNs() int64 {
	var ts unix.Timespec
	_ = unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return int64(ts.Sec)*1_000_000_000 + int64(ts.Nsec)
}

// sleepUntil is the cross-platform interface used by main.go. On
// Linux we drive a monotonic-clock deadline derived from a wall-clock
// time.Time by translating once at first call and then deltaing.
//
// To avoid stateful translation, we accept a wall-clock target,
// compute the delta from time.Now(), then sleep that delta in
// monotonic terms. The result is the same effective semantics as the
// original API but free of the wall/monotonic mismatch bug.
func sleepUntil(target time.Time) error {
	wait := time.Until(target)
	if wait <= 0 {
		return nil
	}
	deadline := nowMonoNs() + wait.Nanoseconds()
	return sleepUntilMono(deadline)
}

// pinSelf calls sched_setaffinity for the calling thread.
func pinSelf(cpuSet string) error {
	cpus, err := parseCPUSet(cpuSet)
	if err != nil {
		return err
	}
	var set unix.CPUSet
	for _, c := range cpus {
		set.Set(c)
	}
	return unix.SchedSetaffinity(0, &set)
}
