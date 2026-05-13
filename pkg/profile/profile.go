// Package profile defines the workload archetypes the chaos-worker
// exposes via --profile. Profiles compose the primitives in
// pkg/workload/ into a steady-state pattern plus a reconcile-burst
// pattern that mirrors a real OpenShift component class.
//
// See ../../../chaos-harness-design.md §3.2 for parameter rationale.
package profile

import (
	"fmt"
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

// Profile is the static description of a workload archetype.
type Profile struct {
	Name        string
	Description string
	SteadyState SteadyConfig
	Reconcile   ReconcileConfig
}

// SteadyConfig parameterises the always-on steady-state workload.
type SteadyConfig struct {
	Goroutines       int
	AllocBytesPerSec int64
	AllocSizeDist    workload.SizeDist
	ChannelOpsPerSec int
	SyscallTarget    workload.SyscallTarget
	SyscallsPerSec   int
	LockContention   workload.ContentionLevel
	LockOpsPerSec    int
}

// ReconcileConfig parameterises the periodic burst.
//
// AllocMultiplier and SyscallMultiplier scale the steady-state rates
// during the burst window. GoroutineSpike adds extra goroutines that
// run for Duration and exit.
type ReconcileConfig struct {
	Period            time.Duration
	Duration          time.Duration
	AllocMultiplier   float64
	GoroutineSpike    int
	SyscallMultiplier float64
}

var registry = map[string]Profile{}

func register(p Profile) {
	registry[p.Name] = p
}

// Get returns the named profile or an error.
func Get(name string) (Profile, error) {
	p, ok := registry[name]
	if !ok {
		return Profile{}, fmt.Errorf("unknown profile %q (known: %v)", name, Names())
	}
	return p, nil
}

// Names returns the registered profile names in insertion order.
func Names() []string {
	out := make([]string, 0, len(registry))
	for _, p := range All() {
		out = append(out, p.Name)
	}
	return out
}

// All returns all registered profiles in a stable order.
func All() []Profile {
	order := []string{
		"control-plane",
		"networking",
		"monitoring",
		"logging",
		"operator-generic",
		"etcd-like",
	}
	out := make([]Profile, 0, len(order))
	for _, n := range order {
		if p, ok := registry[n]; ok {
			out = append(out, p)
		}
	}
	return out
}
