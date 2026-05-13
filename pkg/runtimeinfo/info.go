// Package runtimeinfo probes the Go runtime and the OS for the values
// chaos-worker emits in its startup event: GOMAXPROCS, NumCPU,
// scheduler affinity, cgroup quota and cpuset.
package runtimeinfo

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// Snapshot is the bundle of values captured at startup.
type Snapshot struct {
	GoVersion    string `json:"go_version"`
	GOMAXPROCS   int    `json:"gomaxprocs"`
	NumCPU       int    `json:"num_cpu"`
	Affinity     string `json:"affinity,omitempty"`     // "0-191" form, empty if unknown
	CgroupQuota  string `json:"cgroup_quota,omitempty"` // raw cpu.max line; empty if unknown
	CgroupCPUSet string `json:"cgroup_cpuset,omitempty"`
	PID          int    `json:"pid"`
}

// Capture builds a Snapshot. Fields that cannot be probed on the
// running platform are left empty rather than failing — the harness
// builds on macOS for development but only runs in production on
// RHEL.
func Capture() Snapshot {
	s := Snapshot{
		GoVersion:  runtime.Version(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		NumCPU:     runtime.NumCPU(),
		PID:        os.Getpid(),
	}
	s.Affinity = readAffinity()
	s.CgroupQuota, s.CgroupCPUSet = readCgroup()
	return s
}

// readCgroup returns cpu.max and cpuset.cpus from the unified cgroup
// hierarchy if present (cgroup v2). On non-Linux or non-cgroup
// systems the strings come back empty.
func readCgroup() (quota, cpuset string) {
	cg, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", ""
	}
	// cgroup v2 line: "0::/some/path"
	for _, line := range strings.Split(strings.TrimSpace(string(cg)), "\n") {
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		if parts[0] != "0" {
			continue
		}
		base := "/sys/fs/cgroup" + parts[2]
		if b, err := os.ReadFile(base + "/cpu.max"); err == nil {
			quota = strings.TrimSpace(string(b))
		}
		if b, err := os.ReadFile(base + "/cpuset.cpus.effective"); err == nil {
			cpuset = strings.TrimSpace(string(b))
		}
		return
	}
	return "", ""
}

// FormatCPUSet renders a slice of CPU indexes as a compact range list
// like "0-3,8-11".
func FormatCPUSet(cpus []int) string {
	if len(cpus) == 0 {
		return ""
	}
	var sb strings.Builder
	start := cpus[0]
	prev := start
	flush := func() {
		if sb.Len() > 0 {
			sb.WriteByte(',')
		}
		if start == prev {
			fmt.Fprintf(&sb, "%d", start)
		} else {
			fmt.Fprintf(&sb, "%d-%d", start, prev)
		}
	}
	for _, c := range cpus[1:] {
		if c == prev+1 {
			prev = c
			continue
		}
		flush()
		start = c
		prev = c
	}
	flush()
	return sb.String()
}
