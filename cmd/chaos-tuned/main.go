// Command chaos-tuned reads an OpenShift PerformanceProfile YAML, resolves
// it against a chaos-harness topology, and either prints the resulting plan
// or applies the host-level tuning (preflight + cgroup slice creation +
// hugepage / governor checks).
//
// Subcommands:
//
//	plan       --profile pp.yaml --topology sno.yaml [--run-config run2.yaml]
//	             Print the resolved per-process plan (cpuset, GOMAXPROCS, ...)
//	             as YAML or JSON.
//
//	preflight  --profile pp.yaml
//	             Run host-level checks (kernel cmdline, hugepages, governor,
//	             RT kernel, irqbalance). Exit 0 if all required checks pass,
//	             1 otherwise.
//
//	apply      --profile pp.yaml --topology sno.yaml
//	             Run preflight + create the chaos.slice cgroup and any
//	             one-time host setup. Idempotent. The launcher does this
//	             automatically; this subcommand is for out-of-band setup.
//
//	verify     --plan plan.yaml --pids '<component>=<pid>,...'
//	             Read /proc/<pid>/status for each provided pid and check it
//	             matches the plan's cpuset. Exit 0 on success, 1 on any
//	             mismatch.
//
//	explain    --profile pp.yaml --topology sno.yaml
//	             Render a human-readable mapping of OpenShift component
//	             names → tuning class → cpuset.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/glennswest/chaos-harness/pkg/topology"
	"github.com/glennswest/chaos-harness/pkg/tuning"
	"gopkg.in/yaml.v3"
)

var version = "1.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "plan":
		cmdPlan(os.Args[2:])
	case "preflight":
		cmdPreflight(os.Args[2:])
	case "apply":
		cmdApply(os.Args[2:])
	case "verify":
		cmdVerify(os.Args[2:])
	case "explain":
		cmdExplain(os.Args[2:])
	case "version", "-version", "--version":
		fmt.Println("chaos-tuned", version)
	case "help", "-help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "chaos-tuned: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `chaos-tuned — OpenShift PerformanceProfile → pure-RHEL tuning for chaos-harness

Usage:
  chaos-tuned plan       --profile pp.yaml --topology sno.yaml [--format yaml|json]
  chaos-tuned preflight  --profile pp.yaml
  chaos-tuned apply      --profile pp.yaml --topology sno.yaml [--backend systemd-run|cgroup-v2|taskset]
  chaos-tuned verify     --profile pp.yaml --topology sno.yaml --pids 'kube-apiserver-0=12345,...'
  chaos-tuned explain    --profile pp.yaml --topology sno.yaml
  chaos-tuned version`)
}

func cmdPlan(args []string) {
	fs := flag.NewFlagSet("plan", flag.ExitOnError)
	profPath := fs.String("profile", "", "PerformanceProfile YAML")
	topoPath := fs.String("topology", "", "topology YAML or built-in name (sno|master|worker)")
	format := fs.String("format", "yaml", "output format: yaml|json")
	gomaxprocsOverride := fs.Int("worker-gomaxprocs", 0, "global GOMAXPROCS override (0 = inherit topology)")
	_ = fs.Parse(args)
	if *profPath == "" || *topoPath == "" {
		die("plan: --profile and --topology are required")
	}
	prof, topo := mustLoad(*profPath, *topoPath)
	plan, err := tuning.BuildPlan(prof, topo, topo.Flatten(*gomaxprocsOverride))
	if err != nil {
		die("BuildPlan: %v", err)
	}
	emit := map[string]any{
		"profile":      prof.Metadata.Name,
		"topology":     topo.HostType,
		"reserved":     plan.Reserved.String(),
		"isolated":     plan.Isolated.String(),
		"workload":    plan.WorkloadCPUs.String(),
		"victim_cpus": plan.VictimCPUs.String(),
		"realtime":    plan.RealTime,
		"assignments": planAssignmentsForOutput(plan),
	}
	switch *format {
	case "json":
		_ = json.NewEncoder(os.Stdout).Encode(emit)
	case "yaml", "":
		enc := yaml.NewEncoder(os.Stdout)
		enc.SetIndent(2)
		_ = enc.Encode(emit)
		_ = enc.Close()
	default:
		die("--format must be yaml or json")
	}
}

func cmdPreflight(args []string) {
	fs := flag.NewFlagSet("preflight", flag.ExitOnError)
	profPath := fs.String("profile", "", "PerformanceProfile YAML")
	_ = fs.Parse(args)
	if *profPath == "" {
		die("preflight: --profile is required")
	}
	prof, err := tuning.LoadFile(*profPath)
	if err != nil {
		die("load profile: %v", err)
	}
	report := tuning.Preflight(prof)
	fmt.Println("preflight report:")
	fmt.Print(report.String())
	if failures := report.RequiredFailures(); len(failures) > 0 {
		fmt.Fprintf(os.Stderr, "preflight: %d required check(s) failed\n", len(failures))
		os.Exit(1)
	}
	fmt.Println("preflight: all required checks passed")
}

func cmdApply(args []string) {
	fs := flag.NewFlagSet("apply", flag.ExitOnError)
	profPath := fs.String("profile", "", "PerformanceProfile YAML")
	topoPath := fs.String("topology", "", "topology YAML or built-in name")
	backend := fs.String("backend", "", "applier backend (auto-select if empty)")
	_ = fs.Parse(args)
	if *profPath == "" || *topoPath == "" {
		die("apply: --profile and --topology are required")
	}
	prof, topo := mustLoad(*profPath, *topoPath)
	plan, err := tuning.BuildPlan(prof, topo, topo.Flatten(0))
	if err != nil {
		die("BuildPlan: %v", err)
	}
	report := tuning.Preflight(prof)
	fmt.Println("preflight report:")
	fmt.Print(report.String())
	if fails := report.RequiredFailures(); len(fails) > 0 {
		fmt.Fprintf(os.Stderr, "apply: aborting; %d required preflight check(s) failed\n", len(fails))
		os.Exit(1)
	}
	app, err := tuning.AutoSelect(context.Background(), *backend)
	if err != nil {
		die("AutoSelect: %v", err)
	}
	fmt.Printf("apply: backend=%s\n", app.Name())
	if err := app.PrepareHost(context.Background(), plan); err != nil {
		die("PrepareHost: %v", err)
	}
	fmt.Println("apply: host prepared.")
}

func cmdVerify(args []string) {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	profPath := fs.String("profile", "", "PerformanceProfile YAML")
	topoPath := fs.String("topology", "", "topology YAML or built-in name")
	pidsArg := fs.String("pids", "", "comma-separated component=pid pairs")
	_ = fs.Parse(args)
	if *profPath == "" || *topoPath == "" || *pidsArg == "" {
		die("verify: --profile, --topology, and --pids are required")
	}
	prof, topo := mustLoad(*profPath, *topoPath)
	plan, err := tuning.BuildPlan(prof, topo, topo.Flatten(0))
	if err != nil {
		die("BuildPlan: %v", err)
	}
	pids := map[string]int{}
	for _, kv := range strings.Split(*pidsArg, ",") {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.Index(kv, "=")
		if eq < 0 {
			die("verify: bad --pids entry %q", kv)
		}
		pid, err := strconv.Atoi(strings.TrimSpace(kv[eq+1:]))
		if err != nil {
			die("verify: bad pid in %q", kv)
		}
		pids[strings.TrimSpace(kv[:eq])] = pid
	}
	results := tuning.VerifyAll(plan, pids)
	fmt.Println("verify results:")
	fmt.Print(tuning.FormatVerifyResults(results))
	if tuning.AnyFailed(results) {
		os.Exit(1)
	}
}

func cmdExplain(args []string) {
	fs := flag.NewFlagSet("explain", flag.ExitOnError)
	profPath := fs.String("profile", "", "PerformanceProfile YAML")
	topoPath := fs.String("topology", "", "topology YAML or built-in name")
	_ = fs.Parse(args)
	if *profPath == "" || *topoPath == "" {
		die("explain: --profile and --topology are required")
	}
	prof, topo := mustLoad(*profPath, *topoPath)
	plan, err := tuning.BuildPlan(prof, topo, topo.Flatten(0))
	if err != nil {
		die("BuildPlan: %v", err)
	}
	fmt.Printf("Profile: %s\n", prof.Metadata.Name)
	fmt.Printf("  reserved CPUs:  %s (%d)\n", plan.Reserved, plan.Reserved.Len())
	fmt.Printf("  isolated CPUs:  %s (%d)\n", plan.Isolated, plan.Isolated.Len())
	fmt.Printf("  workload CPUs:  %s (%d)\n", plan.WorkloadCPUs, plan.WorkloadCPUs.Len())
	fmt.Printf("  victim CPUs:    %s (exclusive)\n", plan.VictimCPUs)
	fmt.Printf("  RT kernel:      %v\n", plan.RealTime)
	if plan.NUMATopologyPolicy != "" {
		fmt.Printf("  NUMA policy:    %s\n", plan.NUMATopologyPolicy)
	}
	fmt.Println()
	fmt.Printf("Topology: %s (%d components)\n", topo.HostType, len(topo.Components))
	fmt.Println()
	fmt.Println("Per-process assignments:")
	for _, a := range plan.Assignments {
		who := a.Component
		if a.ReplicaID != "" {
			who = a.ReplicaID
		}
		fmt.Printf("  %-32s class=%-20s cpus=%-12s gomaxprocs=%-3d  %s\n",
			who, a.Class, a.CPUs, a.GOMAXPROCS, a.Source)
	}
}

func mustLoad(profPath, topoPath string) (tuning.PerformanceProfile, topology.Topology) {
	prof, err := tuning.LoadFile(profPath)
	if err != nil {
		die("load profile: %v", err)
	}
	topo, err := topology.LoadFile(topoPath)
	if err != nil {
		// Try interpreting as a built-in name relative to ./topologies/.
		alt := "topologies/" + topoPath + ".yaml"
		t2, err2 := topology.LoadFile(alt)
		if err2 != nil {
			die("load topology %q: %v (also tried %s: %v)", topoPath, err, alt, err2)
		}
		topo = t2
	}
	return prof, topo
}

func planAssignmentsForOutput(p *tuning.Plan) []map[string]any {
	out := make([]map[string]any, 0, len(p.Assignments))
	for _, a := range p.Assignments {
		out = append(out, map[string]any{
			"component":   a.Component,
			"replica_id":  a.ReplicaID,
			"class":       string(a.Class),
			"cpus":        a.CPUs.String(),
			"exclusive":   a.Exclusive,
			"gomaxprocs":  a.GOMAXPROCS,
			"memory_max":  a.MemoryMax,
			"rt_priority": a.RTPriority,
			"source":      a.Source,
		})
	}
	return out
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "chaos-tuned: "+format+"\n", args...)
	os.Exit(1)
}
