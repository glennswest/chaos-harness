package tuning

import (
	"testing"

	"github.com/glennswest/chaos-harness/pkg/topology"
)

func miniTopology() topology.Topology {
	return topology.Topology{
		HostType: "test",
		Components: []topology.Component{
			{Name: "etcd", Processes: []topology.ProcessSpec{{Profile: "etcd-like", Replicas: 1, IdealThreads: 8}}},
			{Name: "kube-apiserver", Processes: []topology.ProcessSpec{{Profile: "control-plane", Replicas: 1, IdealThreads: 8}}},
			{Name: "ovnkube-node", Processes: []topology.ProcessSpec{{Profile: "networking", Replicas: 1, IdealThreads: 4}}},
			{Name: "prometheus-k8s", Processes: []topology.ProcessSpec{{Profile: "monitoring", Replicas: 2, IdealThreads: 4}}},
			{Name: "vector", Processes: []topology.ProcessSpec{{Profile: "logging", Replicas: 1, IdealThreads: 4}}},
		},
	}
}

func TestBuildPlanBasic(t *testing.T) {
	profileYAML := `
apiVersion: performance.openshift.io/v2
kind: PerformanceProfile
metadata: {name: t1}
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    etcd: {class: reserved}
    kube-apiserver: {class: reserved}
    ovnkube-node: {class: isolated-shared}
    prometheus-k8s: {class: burstable}
    vector: {class: best-effort}
    chaos-victim: {class: isolated-exclusive, cpus: "28-31"}
  defaultClass: burstable
`
	prof, err := Parse([]byte(profileYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	topo := miniTopology()
	flat := topo.Flatten(0)

	plan, err := BuildPlan(prof, topo, flat)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}

	// Reserved-class components must have CPUs == reserved pool.
	for _, name := range []string{"etcd", "kube-apiserver"} {
		a, ok := plan.FindAssignment(name, "")
		if !ok {
			t.Fatalf("no assignment for %s", name)
		}
		if a.CPUs.String() != "0-3" {
			t.Errorf("%s cpus = %q, want 0-3", name, a.CPUs)
		}
		if a.GOMAXPROCS != 4 {
			t.Errorf("%s GOMAXPROCS = %d, want 4", name, a.GOMAXPROCS)
		}
	}

	// chaos-victim must be on 28-31 and exclusive.
	v, ok := plan.FindAssignment(SpecialChaosVictim, "")
	if !ok {
		t.Fatal("no victim assignment")
	}
	if v.CPUs.String() != "28-31" {
		t.Errorf("victim cpus = %q, want 28-31", v.CPUs)
	}
	if !v.Exclusive {
		t.Error("victim must be exclusive")
	}
	if v.GOMAXPROCS != 4 {
		t.Errorf("victim GOMAXPROCS = %d, want 4", v.GOMAXPROCS)
	}

	// No worker may have CPUs overlapping with victim.
	for _, a := range plan.Assignments {
		if a.Component == SpecialChaosVictim {
			continue
		}
		if !a.CPUs.Disjoint(v.CPUs) {
			t.Errorf("worker %s/%s cpus %s overlap victim %s", a.Component, a.ReplicaID, a.CPUs, v.CPUs)
		}
	}

	// ovnkube-node should have its 4-CPU slice carved from the front of workload.
	o, ok := plan.FindAssignment("ovnkube-node", "ovnkube-node-0")
	if !ok {
		t.Fatal("no ovnkube-node-0")
	}
	if o.CPUs.String() != "4-7" {
		t.Errorf("ovnkube-node cpus = %q, want 4-7", o.CPUs)
	}

	// Prometheus has 2 replicas — they should land on different slices.
	p0, _ := plan.FindAssignment("prometheus-k8s", "prometheus-k8s-0")
	p1, _ := plan.FindAssignment("prometheus-k8s", "prometheus-k8s-1")
	if p0.CPUs.String() == p1.CPUs.String() {
		t.Errorf("prometheus replicas got same cpus: %s", p0.CPUs)
	}
	if !p0.CPUs.Disjoint(p1.CPUs) {
		t.Errorf("prometheus replicas overlap: %s vs %s", p0.CPUs, p1.CPUs)
	}

	// Vector is best-effort → full workload pool (which is isolated minus victim).
	vec, _ := plan.FindAssignment("vector", "vector-0")
	if vec.CPUs.String() != "4-27" {
		t.Errorf("vector cpus = %q, want 4-27", vec.CPUs)
	}
}

func TestBuildPlanRejectsOverlappingExclusive(t *testing.T) {
	profileYAML := `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    chaos-victim: {class: isolated-exclusive, cpus: "28-31"}
    other:        {class: isolated-exclusive, cpus: "30-31"}
`
	prof, err := Parse([]byte(profileYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	topo := topology.Topology{
		HostType: "test",
		Components: []topology.Component{
			{Name: "other", Processes: []topology.ProcessSpec{{Profile: "control-plane", Replicas: 1, IdealThreads: 2}}},
		},
	}
	_, err = BuildPlan(prof, topo, topo.Flatten(0))
	if err == nil {
		t.Fatal("expected error for overlapping exclusive cpus")
	}
}

func TestBuildPlanDefaultVictimPlacement(t *testing.T) {
	// No explicit chaos-victim entry — planner should still carve a slice.
	profileYAML := `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    etcd: {class: reserved}
  defaultClass: burstable
`
	prof, err := Parse([]byte(profileYAML))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	topo := miniTopology()
	plan, err := BuildPlan(prof, topo, topo.Flatten(0))
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	v, ok := plan.FindAssignment(SpecialChaosVictim, "")
	if !ok {
		t.Fatal("no victim assignment")
	}
	if v.CPUs.Len() != 4 {
		t.Errorf("default victim width = %d, want 4 (cpus=%s)", v.CPUs.Len(), v.CPUs)
	}
	if !v.Exclusive {
		t.Error("default victim must be exclusive")
	}
}
