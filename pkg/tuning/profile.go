package tuning

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// PerformanceProfile is a faithful subset of the OpenShift v2 PerformanceProfile
// CRD (performance.openshift.io/v2), plus a componentMap extension that maps
// real OpenShift component names (kube-apiserver, etcd, ovnkube-node, ...) to
// the tuning class the harness should apply when simulating that component.
//
// The YAML is intentionally OpenShift-shaped: an unmodified PerformanceProfile
// pulled from a cluster (via `oc get performanceprofile -o yaml`) parses into
// this struct as long as a componentMap section is appended. Vanilla
// PerformanceProfile spec controls the host-level pools (reserved/isolated/
// hugepages/RT/IRQ); componentMap controls how chaos-harness components are
// distributed across those pools.
type PerformanceProfile struct {
	APIVersion string   `yaml:"apiVersion,omitempty"`
	Kind       string   `yaml:"kind,omitempty"`
	Metadata   Metadata `yaml:"metadata,omitempty"`
	Spec       Spec     `yaml:"spec"`
}

// Metadata is the standard k8s ObjectMeta subset we care about.
type Metadata struct {
	Name        string            `yaml:"name,omitempty"`
	Annotations map[string]string `yaml:"annotations,omitempty"`
	Labels      map[string]string `yaml:"labels,omitempty"`
}

// Spec is the PerformanceProfile spec (v2) plus our componentMap extension.
type Spec struct {
	CPU                              CPUSpec       `yaml:"cpu"`
	Hugepages                        Hugepages     `yaml:"hugepages,omitempty"`
	RealTimeKernel                   RealTimeKernel `yaml:"realTimeKernel,omitempty"`
	NUMA                             NUMA           `yaml:"numa,omitempty"`
	WorkloadHints                    WorkloadHints  `yaml:"workloadHints,omitempty"`
	NodeSelector                     map[string]string `yaml:"nodeSelector,omitempty"`
	GloballyDisableIrqLoadBalancing  *bool         `yaml:"globallyDisableIrqLoadBalancing,omitempty"`
	AdditionalKernelArgs             []string      `yaml:"additionalKernelArgs,omitempty"`
	Net                              Net           `yaml:"net,omitempty"`

	// ComponentMap is the chaos-harness extension. Keys are OpenShift
	// component names (must match topology component names exactly) and
	// values describe how the component should be tuned.
	ComponentMap map[string]ComponentTuning `yaml:"componentMap,omitempty"`

	// DefaultClass is applied to topology components not listed in
	// ComponentMap. If empty, defaults to "burstable".
	DefaultClass TuningClass `yaml:"defaultClass,omitempty"`
}

// CPUSpec mirrors PerformanceProfile.spec.cpu.
type CPUSpec struct {
	Reserved     string `yaml:"reserved"`             // "0-3"
	Isolated     string `yaml:"isolated"`             // "4-31"
	Shared       string `yaml:"shared,omitempty"`     // 4.16+: overlap pool for mixed pods
	BalanceIsolated *bool `yaml:"balanceIsolated,omitempty"` // default true
	Offlined     string `yaml:"offlined,omitempty"`   // CPUs offlined entirely
}

// Hugepages mirrors PerformanceProfile.spec.hugepages.
type Hugepages struct {
	DefaultHugepagesSize string         `yaml:"defaultHugepagesSize,omitempty"` // "1G" | "2M"
	Pages                []HugepagePage `yaml:"pages,omitempty"`
}

// HugepagePage is one entry under hugepages.pages.
type HugepagePage struct {
	Size  string `yaml:"size"`            // "1G" | "2M"
	Count int    `yaml:"count"`           // page count
	Node  *int   `yaml:"node,omitempty"`  // optional NUMA node binding
}

// RealTimeKernel toggles the RT kernel.
type RealTimeKernel struct {
	Enabled *bool `yaml:"enabled,omitempty"`
}

// NUMA mirrors PerformanceProfile.spec.numa.
type NUMA struct {
	TopologyPolicy string `yaml:"topologyPolicy,omitempty"` // none|best-effort|restricted|single-numa-node
}

// WorkloadHints mirrors PerformanceProfile.spec.workloadHints (4.11+).
type WorkloadHints struct {
	HighPowerConsumption *bool `yaml:"highPowerConsumption,omitempty"`
	RealTime             *bool `yaml:"realTime,omitempty"`
	PerPodPowerManagement *bool `yaml:"perPodPowerManagement,omitempty"`
}

// Net mirrors PerformanceProfile.spec.net.
type Net struct {
	UserLevelNetworking *bool `yaml:"userLevelNetworking,omitempty"`
}

// TuningClass is the policy bucket assigned to a topology component.
//
// The five classes line up with how OpenShift partitions a node: management
// pods land on reserved CPUs (workload partitioning), real-time pods get
// exclusive isolated CPUs (Guaranteed QoS + integer requests), best-effort
// workload pods share isolated CPUs without exclusivity.
type TuningClass string

const (
	// ClassReserved — pin to reserved CPUs (system.slice/kube.slice equivalent).
	// Models OpenShift workload-partitioned management pods.
	ClassReserved TuningClass = "reserved"

	// ClassIsolatedExclusive — exclusive slice carved out of isolated CPUs.
	// No other component shares these CPUs. Models RT/Guaranteed pods.
	// This is the class chaos-victim runs in.
	ClassIsolatedExclusive TuningClass = "isolated-exclusive"

	// ClassIsolatedShared — pinned to a slice of isolated CPUs that may be
	// shared with other shared-class components if isolated capacity is
	// scarce. Models burstable workload pods on tuned hosts.
	ClassIsolatedShared TuningClass = "isolated-shared"

	// ClassBurstable — pinned to a slice of isolated CPUs sized to the
	// component's ideal_threads, but with cpuset.cpus.exclusive=0 so the
	// scheduler can still migrate. Default for unannotated components.
	ClassBurstable TuningClass = "burstable"

	// ClassBestEffort — no pinning at all; component runs across the full
	// isolated pool. Useful for tail/cleanup workloads.
	ClassBestEffort TuningClass = "best-effort"
)

// ComponentTuning describes per-component tuning in the componentMap.
type ComponentTuning struct {
	Class TuningClass `yaml:"class"`

	// CPUs, if set, overrides the planner's automatic CPU assignment.
	// Useful for pinning a specific component to specific cores
	// (e.g. "kube-apiserver: 8-11").
	CPUs string `yaml:"cpus,omitempty"`

	// GOMAXPROCSOverride forces GOMAXPROCS to a specific value for this
	// component. Zero means "use cpuset width" (the recommended default).
	GOMAXPROCSOverride int `yaml:"gomaxprocsOverride,omitempty"`

	// MemoryMaxBytes (cgroup memory.max) for this component. Zero means
	// no limit. Useful when modelling memory-contended environments.
	MemoryMaxBytes int64 `yaml:"memoryMaxBytes,omitempty"`

	// RTPriority sets SCHED_FIFO priority via chrt. Zero leaves the
	// process at SCHED_OTHER. Only meaningful when RealTimeKernel is
	// enabled in spec; planner refuses non-zero values otherwise.
	RTPriority int `yaml:"rtPriority,omitempty"`

	// Notes is free-form documentation, ignored by the planner.
	Notes string `yaml:"notes,omitempty"`
}

// LoadFile reads, parses, and validates a PerformanceProfile YAML.
func LoadFile(path string) (PerformanceProfile, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PerformanceProfile{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(b)
}

// Parse decodes and validates PerformanceProfile YAML bytes.
func Parse(b []byte) (PerformanceProfile, error) {
	var p PerformanceProfile
	if err := yaml.Unmarshal(b, &p); err != nil {
		return PerformanceProfile{}, fmt.Errorf("parse performanceprofile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return PerformanceProfile{}, err
	}
	return p, nil
}

// Validate checks structural invariants. Cross-checks against a topology
// (e.g. componentMap names matching topology component names) are done
// separately by the planner.
func (p PerformanceProfile) Validate() error {
	// apiVersion / kind are tolerated as either present (real OpenShift
	// dump) or absent (chaos-harness extension-only files).
	if p.APIVersion != "" && !strings.HasPrefix(p.APIVersion, "performance.openshift.io/") {
		return fmt.Errorf("apiVersion %q is not performance.openshift.io/*", p.APIVersion)
	}
	if p.Kind != "" && p.Kind != "PerformanceProfile" {
		return fmt.Errorf("kind %q is not PerformanceProfile", p.Kind)
	}

	reserved, err := ParseCPUList(p.Spec.CPU.Reserved)
	if err != nil {
		return fmt.Errorf("spec.cpu.reserved: %w", err)
	}
	isolated, err := ParseCPUList(p.Spec.CPU.Isolated)
	if err != nil {
		return fmt.Errorf("spec.cpu.isolated: %w", err)
	}
	if reserved.Len() == 0 {
		return fmt.Errorf("spec.cpu.reserved is required")
	}
	if isolated.Len() == 0 {
		return fmt.Errorf("spec.cpu.isolated is required")
	}
	if !reserved.Disjoint(isolated) {
		overlap := reserved.Intersect(isolated)
		return fmt.Errorf("spec.cpu.reserved and isolated overlap on %s", overlap)
	}

	if p.Spec.CPU.Shared != "" {
		shared, err := ParseCPUList(p.Spec.CPU.Shared)
		if err != nil {
			return fmt.Errorf("spec.cpu.shared: %w", err)
		}
		all := reserved.Union(isolated)
		if outside := shared.Difference(all); outside.Len() > 0 {
			return fmt.Errorf("spec.cpu.shared has CPUs %s outside reserved+isolated", outside)
		}
	}

	if p.Spec.CPU.Offlined != "" {
		off, err := ParseCPUList(p.Spec.CPU.Offlined)
		if err != nil {
			return fmt.Errorf("spec.cpu.offlined: %w", err)
		}
		if !off.Disjoint(reserved.Union(isolated)) {
			return fmt.Errorf("spec.cpu.offlined overlaps reserved or isolated")
		}
	}

	for _, page := range p.Spec.Hugepages.Pages {
		if page.Size != "1G" && page.Size != "2M" {
			return fmt.Errorf("hugepages.pages.size %q must be 1G or 2M", page.Size)
		}
		if page.Count <= 0 {
			return fmt.Errorf("hugepages.pages.count must be > 0 (got %d for size %s)", page.Count, page.Size)
		}
	}

	if tp := p.Spec.NUMA.TopologyPolicy; tp != "" {
		switch tp {
		case "none", "best-effort", "restricted", "single-numa-node":
		default:
			return fmt.Errorf("numa.topologyPolicy %q invalid", tp)
		}
	}

	rtEnabled := p.Spec.RealTimeKernel.Enabled != nil && *p.Spec.RealTimeKernel.Enabled
	for name, ct := range p.Spec.ComponentMap {
		switch ct.Class {
		case "":
			return fmt.Errorf("componentMap[%q].class is required", name)
		case ClassReserved, ClassIsolatedExclusive, ClassIsolatedShared, ClassBurstable, ClassBestEffort:
		default:
			return fmt.Errorf("componentMap[%q].class %q invalid", name, ct.Class)
		}
		if ct.CPUs != "" {
			cpus, err := ParseCPUList(ct.CPUs)
			if err != nil {
				return fmt.Errorf("componentMap[%q].cpus: %w", name, err)
			}
			// override CPUs must be a subset of reserved+isolated
			all := reserved.Union(isolated)
			if outside := cpus.Difference(all); outside.Len() > 0 {
				return fmt.Errorf("componentMap[%q].cpus %s not in reserved+isolated", name, outside)
			}
		}
		if ct.RTPriority != 0 && !rtEnabled {
			return fmt.Errorf("componentMap[%q].rtPriority requires spec.realTimeKernel.enabled=true", name)
		}
		if ct.RTPriority < 0 || ct.RTPriority > 99 {
			return fmt.Errorf("componentMap[%q].rtPriority %d out of range [0,99]", name, ct.RTPriority)
		}
	}

	if p.Spec.DefaultClass != "" {
		switch p.Spec.DefaultClass {
		case ClassReserved, ClassIsolatedExclusive, ClassIsolatedShared, ClassBurstable, ClassBestEffort:
		default:
			return fmt.Errorf("spec.defaultClass %q invalid", p.Spec.DefaultClass)
		}
	}

	return nil
}

// ReservedCPUs returns the parsed reserved pool. Validate must have passed.
func (p PerformanceProfile) ReservedCPUs() CPUList {
	c, _ := ParseCPUList(p.Spec.CPU.Reserved)
	return c
}

// IsolatedCPUs returns the parsed isolated pool. Validate must have passed.
func (p PerformanceProfile) IsolatedCPUs() CPUList {
	c, _ := ParseCPUList(p.Spec.CPU.Isolated)
	return c
}

// SharedCPUs returns the parsed shared pool (may be empty).
func (p PerformanceProfile) SharedCPUs() CPUList {
	c, _ := ParseCPUList(p.Spec.CPU.Shared)
	return c
}

// RTEnabled is a convenience for the planner.
func (p PerformanceProfile) RTEnabled() bool {
	return p.Spec.RealTimeKernel.Enabled != nil && *p.Spec.RealTimeKernel.Enabled
}

// IRQLoadBalancingDisabled returns true if globallyDisableIrqLoadBalancing is true.
func (p PerformanceProfile) IRQLoadBalancingDisabled() bool {
	return p.Spec.GloballyDisableIrqLoadBalancing != nil && *p.Spec.GloballyDisableIrqLoadBalancing
}

// EffectiveDefaultClass returns DefaultClass or ClassBurstable.
func (p PerformanceProfile) EffectiveDefaultClass() TuningClass {
	if p.Spec.DefaultClass == "" {
		return ClassBurstable
	}
	return p.Spec.DefaultClass
}
