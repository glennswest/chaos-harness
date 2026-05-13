// Package tuning parses OpenShift PerformanceProfile YAML and applies
// the equivalent CPU isolation, hugepage, IRQ, and scheduling tuning
// to chaos-harness processes on a pure RHEL host — no node-tuning
// operator, no MachineConfig, no kubelet involved.
//
// This file: cpulist parsing, range arithmetic, and rendering.
// CPU lists use the same syntax as the kernel cmdline / cpuset / taskset:
//
//	"0-3,8,12-15"  →  [0,1,2,3,8,12,13,14,15]
package tuning

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// CPUList is a deduplicated, sorted set of logical CPU indexes.
type CPUList []int

// ParseCPUList expands "0-3,8,12-15" into a CPUList.
// Empty input returns an empty list, not an error. Whitespace is
// tolerated. Reverse ranges ("5-3") and non-numeric fragments are
// errors. Duplicates are silently deduplicated.
func ParseCPUList(s string) (CPUList, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return CPUList{}, nil
	}
	seen := make(map[int]struct{})
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "-"); i > 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:i]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[i+1:]))
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("cpulist: bad range %q", part)
			}
			if lo > hi {
				return nil, fmt.Errorf("cpulist: reverse range %q", part)
			}
			if lo < 0 {
				return nil, fmt.Errorf("cpulist: negative cpu %q", part)
			}
			for c := lo; c <= hi; c++ {
				seen[c] = struct{}{}
			}
			continue
		}
		c, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("cpulist: bad cpu %q", part)
		}
		if c < 0 {
			return nil, fmt.Errorf("cpulist: negative cpu %d", c)
		}
		seen[c] = struct{}{}
	}
	out := make(CPUList, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	return out, nil
}

// MustParseCPUList is the panicking form for tests and constants.
func MustParseCPUList(s string) CPUList {
	l, err := ParseCPUList(s)
	if err != nil {
		panic(err)
	}
	return l
}

// String renders a CPUList as a compact range list ("0-3,8,12-15").
// Always sorted; an empty list returns "".
func (l CPUList) String() string {
	if len(l) == 0 {
		return ""
	}
	cpus := append(CPUList(nil), l...)
	sort.Ints(cpus)
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

// Contains reports whether l contains cpu.
func (l CPUList) Contains(cpu int) bool {
	for _, c := range l {
		if c == cpu {
			return true
		}
	}
	return false
}

// Intersect returns the CPUs present in both l and other.
func (l CPUList) Intersect(other CPUList) CPUList {
	set := make(map[int]struct{}, len(other))
	for _, c := range other {
		set[c] = struct{}{}
	}
	var out CPUList
	for _, c := range l {
		if _, ok := set[c]; ok {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}

// Difference returns CPUs in l that are not in other.
func (l CPUList) Difference(other CPUList) CPUList {
	set := make(map[int]struct{}, len(other))
	for _, c := range other {
		set[c] = struct{}{}
	}
	var out CPUList
	for _, c := range l {
		if _, ok := set[c]; !ok {
			out = append(out, c)
		}
	}
	sort.Ints(out)
	return out
}

// Union returns the merged sorted unique union.
func (l CPUList) Union(other CPUList) CPUList {
	set := make(map[int]struct{}, len(l)+len(other))
	for _, c := range l {
		set[c] = struct{}{}
	}
	for _, c := range other {
		set[c] = struct{}{}
	}
	out := make(CPUList, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Ints(out)
	return out
}

// Disjoint reports whether l and other share no CPUs.
func (l CPUList) Disjoint(other CPUList) bool {
	return len(l.Intersect(other)) == 0
}

// Take returns the first n CPUs in sorted order, plus the remaining
// list. If n > len(l), it returns (all, empty). Used by the planner to
// carve per-component slices out of the workload pool.
func (l CPUList) Take(n int) (taken, remaining CPUList) {
	if n <= 0 {
		return CPUList{}, append(CPUList(nil), l...)
	}
	cpus := append(CPUList(nil), l...)
	sort.Ints(cpus)
	if n >= len(cpus) {
		return cpus, CPUList{}
	}
	return cpus[:n], cpus[n:]
}

// Len is the number of CPUs.
func (l CPUList) Len() int { return len(l) }
