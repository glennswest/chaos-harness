package tuning

import (
	"fmt"
	"sort"

	"github.com/glennswest/chaos-harness/pkg/topology"
)

// SpecialChaosVictim is the synthetic component name reserved for the
// chaos-victim binary. Profile authors should add an entry under this
// key in componentMap to control where the victim runs.
const SpecialChaosVictim = "chaos-victim"

// SpecialChaosObserver is the synthetic component name for chaos-observer.
const SpecialChaosObserver = "chaos-observer"

// SpecialChaosLauncher is the synthetic component name for chaos-launcher.
const SpecialChaosLauncher = "chaos-launcher"

// Assignment is one tuned process — a worker, the victim, or the observer.
//
// CPUs is the cpuset the process must be confined to. GOMAXPROCS is the
// per-process limit that must be set in the environment (= len(CPUs)
// for everything except reserved-class processes which get NumReserved).
// Exclusive marks cpusets that may not overlap with any other Assignment.
type Assignment struct {
	Component   string      // OpenShift component name, or one of the Special* synthetic names
	ReplicaID   string      // unique within a run (e.g. "kube-apiserver-0"); empty for victim/observer
	Class       TuningClass // tuning class chosen by the planner
	CPUs        CPUList     // resolved cpuset
	Exclusive   bool        // true → cpuset must not overlap with any other Assignment
	GOMAXPROCS  int         // forced GOMAXPROCS value
	MemoryMax   int64       // cgroup memory.max in bytes; 0 = unlimited
	RTPriority  int         // SCHED_FIFO priority; 0 = SCHED_OTHER
	NUMANode    int         // -1 = no NUMA binding
	Source      string      // human-readable explanation ("reserved pool", "carved 4-7 from isolated", ...)
}

// Plan is the resolved tuning plan for a run.
//
// Assignments covers all processes the launcher will spawn (workers +
// victim + observer). Reserved/Isolated/Workload are kept on the plan
// for the appliers to use when configuring slices and cpusets.
type Plan struct {
	Profile     PerformanceProfile
	Topology    topology.Topology
	Reserved    CPUList // same as profile.ReservedCPUs
	Isolated    CPUList // same as profile.IsolatedCPUs
	VictimCPUs  CPUList // exclusive subset of Isolated for chaos-victim
	WorkloadCPUs CPUList // Isolated minus VictimCPUs and any other isolated-exclusive overrides
	Assignments []Assignment

	// Hints captured for downstream appliers.
	HugepagesPages       []HugepagePage
	RealTime             bool
	IRQBannedCPUs        CPUList // CPUs that should have IRQs banned (= isolated)
	NUMATopologyPolicy   string
	HighPowerConsumption bool
}

// BuildPlan produces a Plan from a profile + topology + flattened process
// list. Returns an error on any inconsistency that would make the plan
// unsafe to apply (e.g. requested isolated-exclusive CPUs already taken,
// component override CPUs colliding with reserved, insufficient capacity
// for ideal_threads carve-out).
//
// Algorithm:
//
//  1. Compute reserved + isolated pools from profile.
//  2. Resolve all isolated-exclusive components first (they may have
//     explicit CPUs that carve fixed regions out of isolated).
//  3. Subtract their CPUs from isolated to get the workload pool.
//  4. For each remaining FlatProcess: look up component in componentMap,
//     fall back to DefaultClass, carve a slice from the appropriate pool
//     sized to ideal_threads (or override).
//  5. Assign launcher/observer to reserved pool (shared).
func BuildPlan(profile PerformanceProfile, topo topology.Topology, flat []topology.FlatProcess) (*Plan, error) {
	if err := profile.Validate(); err != nil {
		return nil, fmt.Errorf("invalid profile: %w", err)
	}
	reserved := profile.ReservedCPUs()
	isolated := profile.IsolatedCPUs()

	plan := &Plan{
		Profile:              profile,
		Topology:             topo,
		Reserved:             reserved,
		Isolated:             isolated,
		HugepagesPages:       profile.Spec.Hugepages.Pages,
		RealTime:             profile.RTEnabled(),
		NUMATopologyPolicy:   profile.Spec.NUMA.TopologyPolicy,
		HighPowerConsumption: profile.Spec.WorkloadHints.HighPowerConsumption != nil && *profile.Spec.WorkloadHints.HighPowerConsumption,
	}
	if profile.IRQLoadBalancingDisabled() {
		plan.IRQBannedCPUs = isolated
	}

	// Step 1: resolve all explicit isolated-exclusive overrides.
	// We need a stable iteration order over componentMap to keep the
	// plan deterministic.
	mapNames := make([]string, 0, len(profile.Spec.ComponentMap))
	for n := range profile.Spec.ComponentMap {
		mapNames = append(mapNames, n)
	}
	sort.Strings(mapNames)

	workload := append(CPUList(nil), isolated...)
	exclusiveTaken := make(map[string]CPUList) // component name → CPUs taken
	for _, name := range mapNames {
		ct := profile.Spec.ComponentMap[name]
		if ct.Class != ClassIsolatedExclusive || ct.CPUs == "" {
			continue
		}
		cpus, err := ParseCPUList(ct.CPUs)
		if err != nil {
			return nil, fmt.Errorf("componentMap[%q].cpus: %w", name, err)
		}
		// Must be a subset of isolated.
		if outside := cpus.Difference(isolated); outside.Len() > 0 {
			return nil, fmt.Errorf("componentMap[%q] isolated-exclusive override %s is not in isolated pool %s", name, cpus, isolated)
		}
		// Must not overlap with previously-taken exclusive CPUs.
		for prev, prevCPUs := range exclusiveTaken {
			if !cpus.Disjoint(prevCPUs) {
				return nil, fmt.Errorf("componentMap[%q] exclusive CPUs %s overlap with %q (%s)", name, cpus, prev, prevCPUs)
			}
		}
		exclusiveTaken[name] = cpus
		workload = workload.Difference(cpus)
	}
	plan.WorkloadCPUs = workload

	// Capture victim CPUs explicitly for the launcher; if no override was
	// given, we'll pick the highest-numbered slice of the isolated pool
	// when we get to the chaos-victim assignment below.
	if cpus, ok := exclusiveTaken[SpecialChaosVictim]; ok {
		plan.VictimCPUs = cpus
	}

	// Step 2: build assignments for each flat process.
	// Burstable / shared / best-effort components carve from the
	// `workload` pool sequentially. Reserved-class components share
	// the reserved pool.
	freeWorkload := append(CPUList(nil), workload...)

	// We assign processes in topology order so a single component's
	// replicas can land on adjacent CPUs (better cache locality on a
	// real chiplet host).
	for _, fp := range flat {
		ct, hasMap := profile.Spec.ComponentMap[fp.Component]
		class := ct.Class
		if !hasMap || class == "" {
			class = profile.EffectiveDefaultClass()
		}
		assn := Assignment{
			Component:  fp.Component,
			ReplicaID:  fp.ReplicaID,
			Class:      class,
			NUMANode:   -1,
			MemoryMax:  ct.MemoryMaxBytes,
			RTPriority: ct.RTPriority,
		}

		// Width: how many CPUs this component asks for.
		width := fp.IdealThreads
		if width <= 0 {
			width = 1
		}

		switch class {
		case ClassReserved:
			assn.CPUs = reserved
			assn.GOMAXPROCS = reserved.Len()
			assn.Source = "reserved pool"

		case ClassIsolatedExclusive:
			if ct.CPUs != "" {
				assn.CPUs = exclusiveTaken[fp.Component]
			} else {
				// No explicit CPUs — carve a width-sized slice from the
				// END of the workload pool so it sits on the highest CPU
				// numbers (typically the last NUMA / cache domain, which
				// tends to be the most isolated by default).
				if freeWorkload.Len() < width {
					return nil, fmt.Errorf("component %q: isolated-exclusive needs %d CPUs but workload pool has only %d (%s)",
						fp.Component, width, freeWorkload.Len(), freeWorkload)
				}
				cpus := freeWorkload[freeWorkload.Len()-width:]
				freeWorkload = freeWorkload[:freeWorkload.Len()-width]
				assn.CPUs = append(CPUList(nil), cpus...)
			}
			assn.Exclusive = true
			assn.GOMAXPROCS = assn.CPUs.Len()
			assn.Source = fmt.Sprintf("exclusive carve from isolated (%s)", assn.CPUs)

		case ClassIsolatedShared, ClassBurstable:
			if ct.CPUs != "" {
				cpus, err := ParseCPUList(ct.CPUs)
				if err != nil {
					return nil, fmt.Errorf("componentMap[%q].cpus: %w", fp.Component, err)
				}
				assn.CPUs = cpus
			} else {
				// Carve width-sized slice from front of remaining workload pool.
				if freeWorkload.Len() == 0 {
					// Out of workload CPUs — fall back to the full
					// workload pool. This mirrors how OpenShift treats
					// burstable QoS pods when no exclusive room is left.
					assn.CPUs = workload
				} else {
					take := width
					if take > freeWorkload.Len() {
						take = freeWorkload.Len()
					}
					taken, rest := freeWorkload.Take(take)
					assn.CPUs = taken
					freeWorkload = rest
				}
			}
			assn.GOMAXPROCS = assn.CPUs.Len()
			assn.Source = fmt.Sprintf("%s carve (%s)", class, assn.CPUs)

		case ClassBestEffort:
			// No specific carve — runs across the full workload pool.
			assn.CPUs = workload
			assn.GOMAXPROCS = workload.Len()
			assn.Source = "best-effort: full workload pool"

		default:
			return nil, fmt.Errorf("component %q: unknown class %q", fp.Component, class)
		}

		// Per-component GOMAXPROCS override has the final word. The
		// project's whole point is GOMAXPROCS hygiene, so allow this.
		if ct.GOMAXPROCSOverride > 0 {
			assn.GOMAXPROCS = ct.GOMAXPROCSOverride
		}
		plan.Assignments = append(plan.Assignments, assn)
	}

	// Step 3: synthetic processes (victim, observer, launcher).
	// Launcher and observer go on the reserved pool (they are the
	// chaos-harness equivalent of OpenShift "management" workloads).
	plan.Assignments = append(plan.Assignments, Assignment{
		Component:  SpecialChaosObserver,
		Class:      ClassReserved,
		CPUs:       reserved,
		GOMAXPROCS: reserved.Len(),
		NUMANode:   -1,
		Source:     "observer on reserved pool",
	})

	// Victim placement.
	victimAssn := Assignment{
		Component:  SpecialChaosVictim,
		Class:      ClassIsolatedExclusive,
		Exclusive:  true,
		NUMANode:   -1,
		RTPriority: profile.Spec.ComponentMap[SpecialChaosVictim].RTPriority,
	}
	if vct, ok := profile.Spec.ComponentMap[SpecialChaosVictim]; ok {
		if vct.Class != "" && vct.Class != ClassIsolatedExclusive {
			// Profile author chose a non-exclusive class for the victim.
			// We honor it but emit a warning via Source.
			victimAssn.Class = vct.Class
			victimAssn.Exclusive = vct.Class == ClassIsolatedExclusive
		}
		if vct.CPUs != "" {
			cpus, err := ParseCPUList(vct.CPUs)
			if err != nil {
				return nil, fmt.Errorf("componentMap[chaos-victim].cpus: %w", err)
			}
			victimAssn.CPUs = cpus
		}
	}
	if victimAssn.CPUs.Len() == 0 {
		// Default: carve 4 CPUs from the END of the original isolated
		// pool, NOT freeWorkload. We deliberately want victim CPUs to be
		// disjoint from anything assigned to workers. If the highest
		// isolated CPUs were already given to a worker, we walk down
		// looking for a 4-CPU run that hasn't been assigned.
		const defaultVictimWidth = 4
		used := CPUList{}
		for _, a := range plan.Assignments {
			used = used.Union(a.CPUs)
		}
		candidate := isolated.Difference(used)
		if candidate.Len() < defaultVictimWidth {
			return nil, fmt.Errorf("chaos-victim: cannot find %d isolated CPUs disjoint from worker assignments (free=%s)",
				defaultVictimWidth, candidate)
		}
		victimAssn.CPUs = candidate[candidate.Len()-defaultVictimWidth:]
	}
	victimAssn.GOMAXPROCS = victimAssn.CPUs.Len()
	if victimAssn.Source == "" {
		victimAssn.Source = fmt.Sprintf("victim exclusive on %s", victimAssn.CPUs)
	}
	plan.VictimCPUs = victimAssn.CPUs
	plan.Assignments = append(plan.Assignments, victimAssn)

	// Step 4: post-validation — exclusive sets must be disjoint from
	// every other assignment's CPUs.
	for i, a := range plan.Assignments {
		if !a.Exclusive {
			continue
		}
		for j, b := range plan.Assignments {
			if i == j {
				continue
			}
			if !a.CPUs.Disjoint(b.CPUs) {
				return nil, fmt.Errorf("exclusive component %q (%s) overlaps with %q (%s)",
					a.Component, a.CPUs, b.Component, b.CPUs)
			}
		}
	}

	return plan, nil
}

// FindAssignment returns the first Assignment for the given component and
// replica ID. ReplicaID may be empty to match any replica or for the
// synthetic victim/observer/launcher entries.
func (p *Plan) FindAssignment(component, replicaID string) (Assignment, bool) {
	for _, a := range p.Assignments {
		if a.Component != component {
			continue
		}
		if replicaID != "" && a.ReplicaID != replicaID {
			continue
		}
		return a, true
	}
	return Assignment{}, false
}

// Summary returns a one-line digest of the plan.
func (p *Plan) Summary() string {
	classCounts := map[TuningClass]int{}
	for _, a := range p.Assignments {
		classCounts[a.Class]++
	}
	return fmt.Sprintf("plan: reserved=%s isolated=%s victim=%s assignments=%d (%v)",
		p.Reserved, p.Isolated, p.VictimCPUs, len(p.Assignments), classCounts)
}
