package tuning

import (
	"strings"
	"testing"
)

const realisticTelcoProfile = `
apiVersion: performance.openshift.io/v2
kind: PerformanceProfile
metadata:
  name: sno-telco
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  hugepages:
    defaultHugepagesSize: "1G"
    pages:
      - size: "1G"
        count: 16
  realTimeKernel:
    enabled: true
  numa:
    topologyPolicy: single-numa-node
  globallyDisableIrqLoadBalancing: true
  workloadHints:
    highPowerConsumption: true
    realTime: true
    perPodPowerManagement: false
  componentMap:
    etcd:
      class: reserved
      notes: "fsync-heavy; co-locate with apiserver on reserved"
    kube-apiserver:
      class: reserved
    kube-controller-manager:
      class: reserved
    chaos-victim:
      class: isolated-exclusive
      cpus: "28-31"
      rtPriority: 50
    ovnkube-node:
      class: isolated-shared
    prometheus-k8s:
      class: burstable
    vector:
      class: best-effort
  defaultClass: burstable
`

func TestParseRealistic(t *testing.T) {
	p, err := Parse([]byte(realisticTelcoProfile))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := p.ReservedCPUs().String(); got != "0-3" {
		t.Errorf("reserved = %q, want 0-3", got)
	}
	if got := p.IsolatedCPUs().String(); got != "4-31" {
		t.Errorf("isolated = %q, want 4-31", got)
	}
	if !p.RTEnabled() {
		t.Error("RT should be enabled")
	}
	if !p.IRQLoadBalancingDisabled() {
		t.Error("IRQ load balancing should be disabled")
	}
	if got := p.Spec.NUMA.TopologyPolicy; got != "single-numa-node" {
		t.Errorf("topologyPolicy = %q, want single-numa-node", got)
	}
	if len(p.Spec.Hugepages.Pages) != 1 || p.Spec.Hugepages.Pages[0].Size != "1G" || p.Spec.Hugepages.Pages[0].Count != 16 {
		t.Errorf("hugepages = %#v, want one 1G x 16", p.Spec.Hugepages.Pages)
	}
	if got := p.Spec.ComponentMap["chaos-victim"].Class; got != ClassIsolatedExclusive {
		t.Errorf("chaos-victim class = %q, want isolated-exclusive", got)
	}
	if got := p.Spec.ComponentMap["chaos-victim"].CPUs; got != "28-31" {
		t.Errorf("chaos-victim cpus = %q, want 28-31", got)
	}
	if got := p.Spec.ComponentMap["chaos-victim"].RTPriority; got != 50 {
		t.Errorf("chaos-victim rt prio = %d, want 50", got)
	}
	if p.EffectiveDefaultClass() != ClassBurstable {
		t.Errorf("default class = %v, want burstable", p.EffectiveDefaultClass())
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		msg  string
	}{
		{
			name: "reserved/isolated overlap",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "2-7"
`,
			msg: "overlap",
		},
		{
			name: "missing reserved",
			yaml: `
spec:
  cpu:
    isolated: "4-31"
`,
			msg: "reserved is required",
		},
		{
			name: "missing isolated",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
`,
			msg: "isolated is required",
		},
		{
			name: "bad class",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    foo:
      class: nonsense
`,
			msg: "class \"nonsense\" invalid",
		},
		{
			name: "rt priority without RT kernel",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    foo:
      class: isolated-exclusive
      rtPriority: 50
`,
			msg: "rtPriority requires",
		},
		{
			name: "override cpus outside pools",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  componentMap:
    foo:
      class: burstable
      cpus: "100-103"
`,
			msg: "not in reserved+isolated",
		},
		{
			name: "bad hugepage size",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  hugepages:
    pages:
      - size: "4M"
        count: 4
`,
			msg: "must be 1G or 2M",
		},
		{
			name: "bad numa policy",
			yaml: `
spec:
  cpu:
    reserved: "0-3"
    isolated: "4-31"
  numa:
    topologyPolicy: silly
`,
			msg: "topologyPolicy",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.msg)
			}
			if !strings.Contains(err.Error(), tc.msg) {
				t.Errorf("error = %q, want substring %q", err.Error(), tc.msg)
			}
		})
	}
}
