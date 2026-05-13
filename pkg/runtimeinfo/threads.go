package runtimeinfo

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// ReadThreads returns the value of `Threads:` from /proc/self/status,
// or 0 on any error / non-Linux platform. This is the canonical
// per-process M-count metric; runtime.NumGoroutine misses M's parked
// in syscalls, which is exactly the failure mode this harness models.
func ReadThreads() int {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "Threads:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return 0
		}
		n, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return n
	}
	return 0
}

// ReadRSSBytes returns VmRSS from /proc/self/status in bytes, or 0.
func ReadRSSBytes() int64 {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		// "VmRSS:  84934 kB"
		if len(fields) < 2 {
			return 0
		}
		n, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return n * 1024 // kB → bytes
	}
	return 0
}
