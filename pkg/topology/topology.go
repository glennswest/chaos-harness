// Package topology models a host's OpenShift component layout.
//
// A topology is a list of components; each component has one or more
// processes; each process has a profile (workload archetype),
// GOMAXPROCS limit, and replica count. The launcher consumes a
// topology and spawns one chaos-worker process per
// (component, process-spec, replica-index).
//
// Three built-in topologies model the standard OCP node roles:
//
//	sno     — Single Node OpenShift: full control plane + workload on
//	          one host
//	master  — multinode master: control plane + cluster operators
//	worker  — multinode worker: kubelet, OVN-K node, observability
//	          agents, user workload simulation
//
// Custom topologies are loaded from YAML; built-ins are loaded from
// the topologies/ directory shipped with the repo.
package topology

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Topology is the root document.
type Topology struct {
	HostType    string      `yaml:"host_type"`
	Description string      `yaml:"description,omitempty"`
	Components  []Component `yaml:"components"`
}

// Component groups one or more processes under a logical OCP name
// (e.g. "kube-apiserver", "etcd", "ovn-kubernetes-master").
type Component struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description,omitempty"`
	Role        string         `yaml:"role,omitempty"` // free-form: control-plane, network, storage, monitoring, logging, operator, ...
	Processes   []ProcessSpec  `yaml:"processes"`
}

// ProcessSpec describes one Go-process group within a component.
//
// Replicas is how many independent processes of this spec run on the
// host. GOMAXPROCS is the per-process limit (0 means inherit Go's
// default, which is the failure mode the harness reproduces).
// IdealThreads is documentation only — the comment that captures the
// engineer's view of how many OS threads this component "should" use.
type ProcessSpec struct {
	Profile      string `yaml:"profile"`
	Replicas     int    `yaml:"replicas"`
	GOMAXPROCS   int    `yaml:"gomaxprocs"`
	IdealThreads int    `yaml:"ideal_threads,omitempty"`
	Notes        string `yaml:"notes,omitempty"`
}

// FlatProcess is one resolved process to launch — produced by Flatten.
type FlatProcess struct {
	Component    string
	Profile      string
	GOMAXPROCS   int
	IdealThreads int
	ReplicaID    string // unique within a run, e.g. "etcd-0", "kube-apiserver-1"
}

// Flatten expands a Topology into a list of individual processes,
// applying gomaxprocsOverride to every process if non-zero (used by
// run-config worker_gomaxprocs).
func (t Topology) Flatten(gomaxprocsOverride int) []FlatProcess {
	var out []FlatProcess
	// Per-component replica counter so duplicate component names
	// (rare but allowed) get distinct replica IDs.
	indexes := map[string]int{}
	for _, c := range t.Components {
		for _, p := range c.Processes {
			replicas := p.Replicas
			if replicas <= 0 {
				replicas = 1
			}
			gmp := p.GOMAXPROCS
			if gomaxprocsOverride > 0 {
				gmp = gomaxprocsOverride
			}
			for i := 0; i < replicas; i++ {
				idx := indexes[c.Name]
				indexes[c.Name] = idx + 1
				out = append(out, FlatProcess{
					Component:    c.Name,
					Profile:      p.Profile,
					GOMAXPROCS:   gmp,
					IdealThreads: p.IdealThreads,
					ReplicaID:    fmt.Sprintf("%s-%d", c.Name, idx),
				})
			}
		}
	}
	return out
}

// LoadFile reads a topology YAML from disk and validates it.
func LoadFile(path string) (Topology, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Topology{}, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(b)
}

// Parse decodes and validates topology YAML bytes.
func Parse(b []byte) (Topology, error) {
	var t Topology
	if err := yaml.Unmarshal(b, &t); err != nil {
		return Topology{}, fmt.Errorf("parse topology: %w", err)
	}
	if err := t.Validate(); err != nil {
		return Topology{}, err
	}
	return t, nil
}

// Validate checks structural invariants. Profile names are NOT
// validated here — that's the launcher's job (it has the profile
// registry).
func (t Topology) Validate() error {
	if t.HostType == "" {
		return fmt.Errorf("host_type is required")
	}
	if len(t.Components) == 0 {
		return fmt.Errorf("at least one component is required")
	}
	for i, c := range t.Components {
		if c.Name == "" {
			return fmt.Errorf("component[%d]: name is required", i)
		}
		if len(c.Processes) == 0 {
			return fmt.Errorf("component %q: at least one process is required", c.Name)
		}
		for j, p := range c.Processes {
			if p.Profile == "" {
				return fmt.Errorf("component %q process[%d]: profile is required", c.Name, j)
			}
			if p.Replicas < 0 {
				return fmt.Errorf("component %q process[%d]: replicas must be >= 0", c.Name, j)
			}
			if p.GOMAXPROCS < 0 {
				return fmt.Errorf("component %q process[%d]: gomaxprocs must be >= 0", c.Name, j)
			}
		}
	}
	return nil
}

// Summary returns a one-line digest like
//
//	"sno: 38 components, 47 processes, 1 etcd-like + 23 operator-generic + ..."
func (t Topology) Summary() string {
	totalProcs := 0
	profileCounts := map[string]int{}
	for _, c := range t.Components {
		for _, p := range c.Processes {
			r := p.Replicas
			if r <= 0 {
				r = 1
			}
			totalProcs += r
			profileCounts[p.Profile] += r
		}
	}
	return fmt.Sprintf("%s: %d components, %d processes (%v)",
		t.HostType, len(t.Components), totalProcs, profileCounts)
}
