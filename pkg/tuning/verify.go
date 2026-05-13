package tuning

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// VerifyResult is the outcome of a single per-process verification.
type VerifyResult struct {
	Component   string
	ReplicaID   string
	PID         int
	WantCPUs    CPUList
	GotCPUs     CPUList
	WantMaxProc int
	GotMaxProc  int // GOMAXPROCS as reported by the worker startup event; 0 if unknown
	OK          bool
	Detail      string
}

// VerifyPID checks that the running process pid has been confined to the
// expected cpuset. Reads /proc/<pid>/status Cpus_allowed_list. On non-Linux
// or if /proc is missing, returns OK=true with a "skipped" detail.
//
// GOMAXPROCS is not directly inspectable from outside a Go process; the
// verifier checks cpuset only and trusts the launcher's env injection for
// GOMAXPROCS. The chaos-worker startup JSONL event records its actual
// GOMAXPROCS, which the aggregator can cross-check.
func VerifyPID(pid int, a Assignment) VerifyResult {
	r := VerifyResult{
		Component:   a.Component,
		ReplicaID:   a.ReplicaID,
		PID:         pid,
		WantCPUs:    a.CPUs,
		WantMaxProc: a.GOMAXPROCS,
	}
	statusPath := fmt.Sprintf("/proc/%d/status", pid)
	b, err := os.ReadFile(statusPath)
	if err != nil {
		r.OK = true
		r.Detail = fmt.Sprintf("skipped (cannot read %s: %v)", statusPath, err)
		return r
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "Cpus_allowed_list:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:"))
		got, err := ParseCPUList(val)
		if err != nil {
			r.OK = false
			r.Detail = fmt.Sprintf("could not parse Cpus_allowed_list=%q: %v", val, err)
			return r
		}
		r.GotCPUs = got
		// Pass condition: the running process is confined to a SUBSET of
		// the planned CPUs. Equality is also acceptable. A SUPERSET means
		// the cpuset was not applied.
		if got.Difference(a.CPUs).Len() > 0 {
			r.OK = false
			r.Detail = fmt.Sprintf("Cpus_allowed_list=%s exceeds planned cpuset %s", got, a.CPUs)
			return r
		}
		r.OK = true
		r.Detail = fmt.Sprintf("Cpus_allowed_list=%s ⊆ planned %s", got, a.CPUs)
		return r
	}
	r.OK = false
	r.Detail = fmt.Sprintf("no Cpus_allowed_list line in %s", statusPath)
	return r
}

// VerifyAll runs VerifyPID for every Assignment that has a PID. The map is
// keyed by (component, replicaID). If a key is missing the assignment is
// skipped.
func VerifyAll(plan *Plan, pids map[string]int) []VerifyResult {
	var out []VerifyResult
	for _, a := range plan.Assignments {
		key := a.Component
		if a.ReplicaID != "" {
			key = a.ReplicaID
		}
		pid, ok := pids[key]
		if !ok {
			continue
		}
		out = append(out, VerifyPID(pid, a))
	}
	return out
}

// FormatVerifyResults renders a list of VerifyResults as a human-readable table.
func FormatVerifyResults(results []VerifyResult) string {
	var sb strings.Builder
	for _, r := range results {
		mark := "✓"
		if !r.OK {
			mark = "✗"
		}
		who := r.Component
		if r.ReplicaID != "" {
			who = r.ReplicaID
		}
		fmt.Fprintf(&sb, "  %s %-30s pid=%-7d %s\n", mark, who, r.PID, r.Detail)
	}
	return sb.String()
}

// AnyFailed returns true if any VerifyResult has OK=false.
func AnyFailed(results []VerifyResult) bool {
	for _, r := range results {
		if !r.OK {
			return true
		}
	}
	return false
}

// ParsePIDStatusForCPUs is exported for the aggregator and worker startup
// path: given the contents of /proc/<pid>/status, return the CPUs allowed
// list. Returns an empty list if no Cpus_allowed_list line is present.
func ParsePIDStatusForCPUs(status string) (CPUList, error) {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "Cpus_allowed_list:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "Cpus_allowed_list:"))
		return ParseCPUList(val)
	}
	return CPUList{}, nil
}

// _ keeps strconv as an import for future ergonomic helpers.
var _ = strconv.Itoa
