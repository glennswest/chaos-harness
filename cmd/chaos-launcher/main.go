// Command chaos-launcher reads a run-config YAML, resolves the
// topology, spawns the configured worker mix plus a victim and
// observer, runs to completion, then invokes the aggregator.
//
// See ../../README.md and ../../../chaos-harness-design.md §4.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/glennswest/chaos-harness/pkg/profile"
	"github.com/glennswest/chaos-harness/pkg/runconfig"
	chaossync "github.com/glennswest/chaos-harness/pkg/sync"
	"github.com/glennswest/chaos-harness/pkg/topology"
	"github.com/glennswest/chaos-harness/pkg/tuning"
	"gopkg.in/yaml.v3"
)

var version = "1.1.0"

type cliConfig struct {
	configPath  string
	outputDir   string
	binDir      string
	skipAggregate bool
	showVersion bool
}

func parseFlags() (*cliConfig, error) {
	c := &cliConfig{}
	flag.StringVar(&c.configPath, "config", "", "path to run-config YAML")
	flag.StringVar(&c.outputDir, "output-dir", "results/", "results parent directory; per-run dir is created under this")
	flag.StringVar(&c.binDir, "bin-dir", "", "directory containing chaos-worker/-victim/-observer binaries; default: same dir as chaos-launcher")
	flag.BoolVar(&c.skipAggregate, "skip-aggregate", false, "skip invoking scripts/aggregate-results.py at end of run")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()
	if c.showVersion {
		return c, nil
	}
	if c.configPath == "" {
		return nil, fmt.Errorf("--config is required")
	}
	return c, nil
}

func main() {
	cli, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-launcher:", err)
		flag.Usage()
		os.Exit(2)
	}
	if cli.showVersion {
		fmt.Println("chaos-launcher", version)
		return
	}

	rc, err := runconfig.LoadFile(cli.configPath)
	if err != nil {
		fatal(err)
	}

	topo, err := loadTopology(rc.Topology)
	if err != nil {
		fatal(err)
	}
	if err := validateProfiles(topo); err != nil {
		fatal(err)
	}
	flat := topo.Flatten(rc.WorkerGOMAXPROCS)

	runDir := filepath.Join(cli.outputDir, rc.RunID)
	rawDir := filepath.Join(runDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		fatal(err)
	}

	binDir := cli.binDir
	if binDir == "" {
		binDir = filepath.Dir(self())
	}
	for _, name := range []string{"chaos-worker", "chaos-victim", "chaos-observer"} {
		if _, err := os.Stat(filepath.Join(binDir, name)); err != nil {
			fatal(fmt.Errorf("missing binary %s in %s (set --bin-dir): %w", name, binDir, err))
		}
	}

	fmt.Printf("chaos-launcher %s\n", version)
	fmt.Printf("  run_id=%s duration=%s mode=%s topology=%s gomaxprocs_override=%d\n",
		rc.RunID, rc.Duration, rc.Mode, rc.Topology, rc.WorkerGOMAXPROCS)
	fmt.Printf("  topology: %s\n", topo.Summary())
	fmt.Printf("  spawning %d worker processes + 1 victim + 1 observer\n", len(flat))

	// Optional tuning: load PerformanceProfile, preflight host, build plan,
	// pick applier. Strict mode: any failure aborts the run.
	tuningCtx, cancelTuning := context.WithCancel(context.Background())
	defer cancelTuning()
	var tuningPlan *tuning.Plan
	var tuningApplier tuning.Applier
	if rc.PerformanceProfile != "" {
		prof, err := tuning.LoadFile(rc.PerformanceProfile)
		if err != nil {
			fatal(fmt.Errorf("tuning: load profile: %w", err))
		}
		report := tuning.Preflight(prof)
		fmt.Println("  preflight:")
		fmt.Print("    " + strings.ReplaceAll(report.String(), "\n  ", "\n    "))
		if fails := report.RequiredFailures(); len(fails) > 0 {
			fatal(fmt.Errorf("tuning: %d required preflight check(s) failed; aborting (strict mode)", len(fails)))
		}
		tuningPlan, err = tuning.BuildPlan(prof, topo, flat)
		if err != nil {
			fatal(fmt.Errorf("tuning: build plan: %w", err))
		}
		fmt.Printf("  %s\n", tuningPlan.Summary())
		tuningApplier, err = tuning.AutoSelect(tuningCtx, rc.TuningBackend)
		if err != nil {
			fatal(fmt.Errorf("tuning: %w", err))
		}
		fmt.Printf("  tuning backend: %s\n", tuningApplier.Name())
		if err := tuningApplier.PrepareHost(tuningCtx, tuningPlan); err != nil {
			fatal(fmt.Errorf("tuning: PrepareHost: %w", err))
		}
		// Persist plan beside manifest.yaml.
		if err := writePlanYAML(runDir, tuningPlan); err != nil {
			fatal(err)
		}
	}

	// Write a manifest of the run for the aggregator.
	if err := writeManifest(runDir, rc, topo, flat); err != nil {
		fatal(err)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(os.Stderr, "chaos-launcher: signal received; tearing down")
			rootCancel()
		case <-rootCtx.Done():
		}
	}()

	// Sync server (only if mode=sync).
	var syncSocket string
	var syncSrv *chaossync.Server
	if rc.Mode == "sync" {
		syncSocket = filepath.Join(os.TempDir(), fmt.Sprintf("chaos-sync-%s.sock", rc.RunID))
		syncSrv, err = chaossync.NewServer(syncSocket)
		if err != nil {
			fatal(err)
		}
		defer syncSrv.Close()
	}

	// Spawn observer + victim first so they capture worker startup.
	procs := []*managedProc{}
	procs = append(procs, spawnObserver(rootCtx, binDir, rawDir, rc, tuningPlan, tuningApplier))
	procs = append(procs, spawnVictim(rootCtx, binDir, rawDir, rc, tuningPlan, tuningApplier))
	for _, fp := range flat {
		procs = append(procs, spawnWorker(rootCtx, binDir, rawDir, rc, fp, syncSocket, tuningPlan, tuningApplier))
	}

	// Strict-mode post-spawn verify: every Assignment must land in its
	// planned cpuset. Wait briefly for processes to settle.
	if tuningPlan != nil {
		time.Sleep(500 * time.Millisecond)
		pids := map[string]int{}
		for _, p := range procs {
			if p.cmd == nil || p.cmd.Process == nil {
				continue
			}
			// Map by name slot used in spawn*: "worker[<replicaID>]",
			// "victim", "observer". Strip the wrapping for verify keys.
			name := p.name
			if strings.HasPrefix(name, "worker[") && strings.HasSuffix(name, "]") {
				pids[strings.TrimSuffix(strings.TrimPrefix(name, "worker["), "]")] = p.cmd.Process.Pid
			} else if name == "victim" {
				pids[tuning.SpecialChaosVictim] = p.cmd.Process.Pid
			} else if name == "observer" {
				pids[tuning.SpecialChaosObserver] = p.cmd.Process.Pid
			}
		}
		// systemd-run scopes spawn an intermediate process; the worker is
		// the child. /proc/<pid>/status of the systemd-run pid will show
		// whatever cpuset the parent had, so we walk one level of children
		// when systemd backend is in use. For cgroup-v2 (sh -c wrapper)
		// the same applies because sh exec's the inner. To keep this
		// simple and backend-agnostic, we recursively descend and pick
		// the deepest descendant matching chaos-worker/-victim/-observer.
		pids = resolveInnerPIDs(pids)
		results := tuning.VerifyAll(tuningPlan, pids)
		fmt.Println("verify:")
		fmt.Print(tuning.FormatVerifyResults(results))
		if tuning.AnyFailed(results) {
			rootCancel()
			terminate(procs, 5*time.Second)
			fatal(fmt.Errorf("tuning: verify failed; aborting (strict mode)"))
		}
	}

	// Sync trigger goroutine.
	if rc.Mode == "sync" && syncSrv != nil {
		go func() {
			select {
			case <-rootCtx.Done():
				return
			case <-time.After(rc.SyncTrigger.InitialOffset):
			}
			t := time.NewTicker(rc.SyncTrigger.Period)
			defer t.Stop()
			for {
				select {
				case <-rootCtx.Done():
					return
				case <-t.C:
					syncSrv.Trigger()
				}
			}
		}()
	}

	// Wait for duration.
	select {
	case <-rootCtx.Done():
	case <-time.After(rc.Duration):
	}

	fmt.Println("chaos-launcher: duration elapsed; sending SIGTERM")
	terminate(procs, 10*time.Second)

	if !cli.skipAggregate {
		invokeAggregator(runDir, cli.configPath)
	}
	fmt.Printf("chaos-launcher: done. results in %s\n", runDir)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chaos-launcher:", err)
	os.Exit(1)
}

func self() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return p
}

// loadTopology accepts a built-in name (sno|master|worker) or a file
// path. Built-ins are looked up under topologies/<name>.yaml relative
// to the binary directory and the current working directory.
func loadTopology(name string) (topology.Topology, error) {
	if _, err := os.Stat(name); err == nil {
		return topology.LoadFile(name)
	}
	candidates := []string{
		filepath.Join("topologies", name+".yaml"),
		filepath.Join(filepath.Dir(self()), "..", "topologies", name+".yaml"),
		filepath.Join(filepath.Dir(self()), "topologies", name+".yaml"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return topology.LoadFile(p)
		}
	}
	return topology.Topology{}, fmt.Errorf("topology %q: not found as file and no built-in (looked in %v)", name, candidates)
}

func validateProfiles(t topology.Topology) error {
	known := map[string]bool{}
	for _, p := range profile.All() {
		known[p.Name] = true
	}
	for _, c := range t.Components {
		for _, p := range c.Processes {
			if !known[p.Profile] {
				return fmt.Errorf("topology component %q: unknown profile %q (known: %v)", c.Name, p.Profile, profile.Names())
			}
		}
	}
	return nil
}

// managedProc bundles a child cmd with bookkeeping for clean shutdown.
type managedProc struct {
	name string
	cmd  *exec.Cmd
	done chan struct{}
}

func spawnWorker(ctx context.Context, binDir, rawDir string, rc runconfig.Config, fp topology.FlatProcess, syncSocket string, plan *tuning.Plan, app tuning.Applier) *managedProc {
	args := []string{
		"--profile=" + fp.Profile,
		"--run-id=" + rc.RunID,
		"--output-dir=" + rawDir,
		"--mode=" + rc.Mode,
		"--duration=" + rc.Duration.String(),
		"--replica-id=" + fp.ReplicaID,
		"--component=" + fp.Component,
	}
	if syncSocket != "" {
		args = append(args, "--sync-socket="+syncSocket)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "chaos-worker"), args...)
	env := os.Environ()
	if fp.GOMAXPROCS > 0 {
		env = append(env, "GOMAXPROCS="+strconv.Itoa(fp.GOMAXPROCS))
	}
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if plan != nil && app != nil {
		if a, ok := plan.FindAssignment(fp.Component, fp.ReplicaID); ok {
			wrapped, err := app.Wrap(ctx, cmd, a)
			if err != nil {
				fatal(fmt.Errorf("tuning: wrap worker %s: %w", fp.ReplicaID, err))
			}
			cmd = wrapped
		}
	}
	return startProc(fmt.Sprintf("worker[%s]", fp.ReplicaID), cmd)
}

func spawnVictim(ctx context.Context, binDir, rawDir string, rc runconfig.Config, plan *tuning.Plan, app tuning.Applier) *managedProc {
	args := []string{
		"--mode=" + rc.Victim.Mode,
		"--output-dir=" + rawDir,
		"--run-id=" + rc.RunID,
		"--duration=" + rc.Duration.String(),
	}
	pinning := rc.Victim.PinningCPUs
	// When a plan is in effect, the planner's victim cpuset overrides
	// the run-config's --pinning-cpus value. Two reasons: (1) the plan
	// guarantees disjoint-from-workers; (2) we want a single source of
	// truth for what the spike isolation invariant actually says.
	if plan != nil {
		pinning = plan.VictimCPUs.String()
	}
	if pinning != "" {
		args = append(args, "--pinning-cpus="+pinning)
	}
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "chaos-victim"), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if plan != nil && app != nil {
		if a, ok := plan.FindAssignment(tuning.SpecialChaosVictim, ""); ok {
			wrapped, err := app.Wrap(ctx, cmd, a)
			if err != nil {
				fatal(fmt.Errorf("tuning: wrap victim: %w", err))
			}
			cmd = wrapped
		}
	}
	return startProc("victim", cmd)
}

func spawnObserver(ctx context.Context, binDir, rawDir string, rc runconfig.Config, plan *tuning.Plan, app tuning.Applier) *managedProc {
	args := []string{
		"--output-dir=" + rawDir,
		"--run-id=" + rc.RunID,
		"--duration=" + rc.Duration.String(),
		"--sample-interval=" + rc.Observer.SampleInterval.String(),
		"--worker-pid-filter=" + rc.Observer.WorkerPIDFilter,
	}
	cmd := exec.CommandContext(ctx, filepath.Join(binDir, "chaos-observer"), args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if plan != nil && app != nil {
		if a, ok := plan.FindAssignment(tuning.SpecialChaosObserver, ""); ok {
			wrapped, err := app.Wrap(ctx, cmd, a)
			if err != nil {
				fatal(fmt.Errorf("tuning: wrap observer: %w", err))
			}
			cmd = wrapped
		}
	}
	return startProc("observer", cmd)
}

func startProc(name string, cmd *exec.Cmd) *managedProc {
	mp := &managedProc{name: name, cmd: cmd, done: make(chan struct{})}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "chaos-launcher: %s start failed: %v\n", name, err)
		close(mp.done)
		return mp
	}
	go func() {
		_ = cmd.Wait()
		close(mp.done)
	}()
	return mp
}

func terminate(procs []*managedProc, grace time.Duration) {
	for _, p := range procs {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Signal(syscall.SIGTERM)
		}
	}
	deadline := time.After(grace)
	var wg sync.WaitGroup
	wg.Add(len(procs))
	for _, p := range procs {
		go func(p *managedProc) {
			defer wg.Done()
			select {
			case <-p.done:
			case <-deadline:
				if p.cmd.Process != nil {
					fmt.Fprintf(os.Stderr, "chaos-launcher: %s did not exit in %s; SIGKILL\n", p.name, grace)
					_ = p.cmd.Process.Kill()
				}
				<-p.done
			}
		}(p)
	}
	wg.Wait()
}

// writeManifest serialises the resolved run + topology + per-process
// expansion into runDir/manifest.yaml so the aggregator and humans can
// read what was actually launched.
func writeManifest(runDir string, rc runconfig.Config, topo topology.Topology, flat []topology.FlatProcess) error {
	m := map[string]any{
		"run_id":             rc.RunID,
		"duration":           rc.Duration.String(),
		"mode":               rc.Mode,
		"topology_name":      rc.Topology,
		"topology_host_type": topo.HostType,
		"worker_gomaxprocs":  rc.WorkerGOMAXPROCS,
		"victim":             rc.Victim,
		"observer":           rc.Observer,
		"sync_trigger":       rc.SyncTrigger,
		"process_count":      len(flat),
		"processes":          flat,
		"started_at":         time.Now().UTC().Format(time.RFC3339),
		"launcher_version":   version,
	}
	path := filepath.Join(runDir, "manifest.yaml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := yaml.NewEncoder(f)
	defer enc.Close()
	enc.SetIndent(2)
	return enc.Encode(m)
}

// writePlanYAML serialises the resolved tuning plan beside manifest.yaml
// so the aggregator and humans can see what the launcher resolved.
func writePlanYAML(runDir string, plan *tuning.Plan) error {
	out := map[string]any{
		"reserved":     plan.Reserved.String(),
		"isolated":     plan.Isolated.String(),
		"workload":     plan.WorkloadCPUs.String(),
		"victim_cpus":  plan.VictimCPUs.String(),
		"realtime":     plan.RealTime,
		"numa_policy":  plan.NUMATopologyPolicy,
		"hugepages":    plan.HugepagesPages,
		"assignments":  assignmentsForOutput(plan),
	}
	path := filepath.Join(runDir, "tuning-plan.yaml")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := yaml.NewEncoder(f)
	defer enc.Close()
	enc.SetIndent(2)
	return enc.Encode(out)
}

func assignmentsForOutput(p *tuning.Plan) []map[string]any {
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

// resolveInnerPIDs walks /proc to translate wrapper PIDs (systemd-run /
// sh) into the inner chaos-worker / -victim / -observer PIDs whose
// cpuset is the one we actually want to verify. If a child cannot be
// found (process exited, /proc unavailable on macOS), the original PID
// stays in the map and the verifier will report "skipped".
func resolveInnerPIDs(pids map[string]int) map[string]int {
	out := make(map[string]int, len(pids))
	for k, parent := range pids {
		out[k] = findInnerChaosPID(parent)
	}
	return out
}

func findInnerChaosPID(parent int) int {
	// Read /proc/*/stat lines to build a parent→child index. Look for
	// a descendant whose comm starts with "chaos-".
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return parent
	}
	type info struct {
		ppid int
		comm string
	}
	procs := map[int]info{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		commBytes, err := os.ReadFile("/proc/" + e.Name() + "/comm")
		if err != nil {
			continue
		}
		statBytes, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		// stat format: "pid (comm) state ppid ..."; comm may contain spaces.
		// Find the closing ')' and tokenise after it.
		s := string(statBytes)
		if i := lastIndex(s, ")"); i > 0 && i+1 < len(s) {
			toks := splitFields(s[i+1:])
			if len(toks) >= 2 {
				ppid, _ := strconv.Atoi(toks[1])
				procs[pid] = info{ppid: ppid, comm: trimNL(string(commBytes))}
			}
		}
	}
	// BFS: find descendants of parent.
	visit := []int{parent}
	for len(visit) > 0 {
		var next []int
		for _, p := range visit {
			for pid, inf := range procs {
				if inf.ppid != p {
					continue
				}
				if hasPrefix(inf.comm, "chaos-") {
					return pid
				}
				next = append(next, pid)
			}
		}
		visit = next
	}
	return parent
}

func lastIndex(s, sub string) int {
	for i := len(s) - len(sub); i >= 0; i-- {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func splitFields(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' || c == '\n' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
			continue
		}
		cur += string(c)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func trimNL(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}

func hasPrefix(s, p string) bool {
	if len(s) < len(p) {
		return false
	}
	return s[:len(p)] == p
}

func invokeAggregator(runDir, configPath string) {
	script := "scripts/aggregate-results.py"
	if _, err := os.Stat(script); err != nil {
		// Look beside the binary too.
		alt := filepath.Join(filepath.Dir(self()), "..", "scripts", "aggregate-results.py")
		if _, err2 := os.Stat(alt); err2 == nil {
			script = alt
		} else {
			fmt.Fprintln(os.Stderr, "chaos-launcher: aggregator script not found; skipping")
			return
		}
	}
	cmd := exec.Command("python3", script, "--run-dir", runDir, "--config", configPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "chaos-launcher: aggregator failed:", err)
	}
}
