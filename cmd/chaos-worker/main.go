// Command chaos-worker runs a single profile workload as one of N
// independent Go processes contending for a node's resources.
//
// See ../../README.md and ../../../chaos-harness-design.md §3.
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"sync"
	"syscall"
	"time"

	"github.com/glennswest/chaos-harness/pkg/output"
	"github.com/glennswest/chaos-harness/pkg/profile"
	"github.com/glennswest/chaos-harness/pkg/runtimeinfo"
	chaossync "github.com/glennswest/chaos-harness/pkg/sync"
	"github.com/glennswest/chaos-harness/pkg/workload"
)

var version = "1.1.0"

type config struct {
	profile     string
	runID       string
	outputDir   string
	mode        string
	syncSocket  string
	duration    time.Duration
	replicaID   string
	component   string
	showVersion bool
}

func parseFlags() (*config, error) {
	c := &config{}
	flag.StringVar(&c.profile, "profile", "", "profile name (control-plane, networking, monitoring, logging, operator-generic, etcd-like)")
	flag.StringVar(&c.runID, "run-id", "", "run identifier; embedded in output filenames")
	flag.StringVar(&c.outputDir, "output-dir", "", "directory for JSONL output")
	flag.StringVar(&c.mode, "mode", "drift", "reconcile trigger mode: drift|sync")
	flag.StringVar(&c.syncSocket, "sync-socket", "", "Unix socket path for sync-mode trigger (sync mode only)")
	flag.DurationVar(&c.duration, "duration", 600*time.Second, "run duration before clean exit")
	flag.StringVar(&c.replicaID, "replica-id", "", "unique replica identifier within a profile group")
	flag.StringVar(&c.component, "component", "", "OCP component name this process simulates (informational; from topology)")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()
	if c.showVersion {
		return c, nil
	}
	if c.profile == "" {
		return nil, fmt.Errorf("--profile is required")
	}
	if c.runID == "" {
		return nil, fmt.Errorf("--run-id is required")
	}
	if c.outputDir == "" {
		return nil, fmt.Errorf("--output-dir is required")
	}
	if c.mode != "drift" && c.mode != "sync" {
		return nil, fmt.Errorf("--mode must be drift or sync, got %q", c.mode)
	}
	if c.mode == "sync" && c.syncSocket == "" {
		return nil, fmt.Errorf("--sync-socket is required in sync mode")
	}
	return c, nil
}

func main() {
	c, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-worker:", err)
		flag.Usage()
		os.Exit(2)
	}
	if c.showVersion {
		fmt.Println("chaos-worker", version)
		return
	}

	prof, err := profile.Get(c.profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-worker:", err)
		os.Exit(2)
	}

	if err := os.MkdirAll(c.outputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "chaos-worker:", err)
		os.Exit(2)
	}
	outPath := filepath.Join(c.outputDir, fmt.Sprintf("worker-%s-%s-%d.jsonl", c.profile, c.replicaID, os.Getpid()))
	w, err := output.NewWriter(outPath, 4096)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-worker:", err)
		os.Exit(2)
	}

	rt := runtimeinfo.Capture()
	w.Emit(map[string]any{
		"ts":            now(),
		"type":          "startup",
		"profile":       c.profile,
		"replica_id":    c.replicaID,
		"component":     c.component,
		"run_id":        c.runID,
		"mode":          c.mode,
		"duration_sec":  c.duration.Seconds(),
		"pid":           rt.PID,
		"go_version":    rt.GoVersion,
		"gomaxprocs":    rt.GOMAXPROCS,
		"num_cpu":       rt.NumCPU,
		"affinity":      rt.Affinity,
		"cgroup_quota":  rt.CgroupQuota,
		"cgroup_cpuset": rt.CgroupCPUSet,
		"version":       version,
	})

	ctx, cancel := context.WithTimeout(context.Background(), c.duration)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-ctx.Done():
		}
	}()

	// Steady-state and observability goroutines.
	var wg sync.WaitGroup
	startSteadyState(ctx, &wg, prof)
	startObserver(ctx, &wg, w)
	startGCWatcher(ctx, &wg, w)
	startReconcileCoordinator(ctx, &wg, w, c, prof)

	wg.Wait()

	w.Emit(map[string]any{
		"ts":     now(),
		"type":   "shutdown",
		"reason": shutdownReason(ctx),
	})
	if dropped, err := w.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "chaos-worker: close output: %v (dropped=%d)\n", err, dropped)
		os.Exit(1)
	} else if dropped > 0 {
		fmt.Fprintf(os.Stderr, "chaos-worker: warning: %d events dropped\n", dropped)
	}
}

func now() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}

func shutdownReason(ctx context.Context) string {
	if ctx.Err() == context.DeadlineExceeded {
		return "duration"
	}
	return "sigterm"
}

// startSteadyState launches the four primitives at the profile's
// steady-state rates. Each runs in its own goroutine pool internally.
func startSteadyState(ctx context.Context, wg *sync.WaitGroup, p profile.Profile) {
	s := p.SteadyState
	wg.Add(4)
	go func() {
		defer wg.Done()
		workload.RunScheduler(ctx, workload.SchedulerConfig{
			Pairs:     s.Goroutines / 4,
			OpsPerSec: s.ChannelOpsPerSec,
		})
	}()
	go func() {
		defer wg.Done()
		workload.RunAlloc(ctx, workload.AllocConfig{
			Goroutines:  max1(s.Goroutines / 8),
			BytesPerSec: s.AllocBytesPerSec,
			SizeDist:    s.AllocSizeDist,
		})
	}()
	go func() {
		defer wg.Done()
		_ = workload.RunSyscall(ctx, workload.SyscallConfig{
			Target:         s.SyscallTarget,
			Goroutines:     max1(s.Goroutines / 8),
			SyscallsPerSec: s.SyscallsPerSec,
		})
	}()
	go func() {
		defer wg.Done()
		workload.RunLock(ctx, workload.LockConfig{
			Level:      s.LockContention,
			Goroutines: max1(s.Goroutines / 8),
			OpsPerSec:  s.LockOpsPerSec,
		})
	}()
}

func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}

// startObserver emits a 1 Hz "sample" event with goroutine count, OS
// thread count, RSS, and GC pause stats.
func startObserver(ctx context.Context, wg *sync.WaitGroup, w *output.Writer) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		t := time.NewTicker(time.Second)
		defer t.Stop()
		samples := []metrics.Sample{
			{Name: "/gc/pauses:seconds"},
			{Name: "/gc/cycles/total:gc-cycles"},
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				metrics.Read(samples)
				p99ns := uint64(0)
				if h := samples[0].Value.Float64Histogram(); h != nil && len(h.Buckets) > 1 {
					p99ns = uint64(percentile(h, 0.99) * 1e9)
				}
				gcCount := samples[1].Value.Uint64()
				w.Emit(map[string]any{
					"ts":              now(),
					"type":            "sample",
					"goroutines":      runtime.NumGoroutine(),
					"threads":         runtimeinfo.ReadThreads(),
					"rss_bytes":       runtimeinfo.ReadRSSBytes(),
					"gc_pause_ns_p99": p99ns,
					"gc_count":        gcCount,
				})
			}
		}
	}()
}

// percentile returns an approximate quantile from a Float64Histogram
// (runtime/metrics format: Counts[i] is samples in bucket [Buckets[i],
// Buckets[i+1])).
func percentile(h *metrics.Float64Histogram, q float64) float64 {
	if h == nil {
		return 0
	}
	var total uint64
	for _, c := range h.Counts {
		total += c
	}
	if total == 0 {
		return 0
	}
	target := uint64(float64(total) * q)
	var seen uint64
	for i, c := range h.Counts {
		seen += c
		if seen >= target {
			// Use upper bound of the bucket.
			if i+1 < len(h.Buckets) {
				return h.Buckets[i+1]
			}
			return h.Buckets[i]
		}
	}
	return h.Buckets[len(h.Buckets)-1]
}

// startGCWatcher polls runtime/metrics at 100 Hz and emits gc_start/
// gc_end events derived from the cycle counter delta. Polling is
// chosen over runtime/trace flight recorder for v1 simplicity (design
// §12.1 accepts this as the v1 approach).
func startGCWatcher(ctx context.Context, wg *sync.WaitGroup, w *output.Writer) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		samples := []metrics.Sample{
			{Name: "/gc/cycles/total:gc-cycles"},
			{Name: "/gc/pauses:seconds"},
		}
		var lastCount uint64
		var lastBucketsTotal uint64
		t := time.NewTicker(10 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				metrics.Read(samples)
				count := samples[0].Value.Uint64()
				if count > lastCount {
					// One or more cycles elapsed since last poll.
					var pauseNs uint64
					if h := samples[1].Value.Float64Histogram(); h != nil && len(h.Buckets) > 1 {
						var totalNow uint64
						for _, c := range h.Counts {
							totalNow += c
						}
						_ = totalNow - lastBucketsTotal
						lastBucketsTotal = totalNow
						pauseNs = uint64(percentile(h, 0.99) * 1e9)
					}
					w.Emit(map[string]any{
						"ts":       now(),
						"type":     "gc_event",
						"cycles":   count - lastCount,
						"pause_ns": pauseNs,
					})
					lastCount = count
				}
			}
		}
	}()
}

// startReconcileCoordinator drives the periodic burst pattern. In
// drift mode it ticks at the profile's reconcile period with a random
// initial offset; in sync mode it waits for RECONCILE messages from
// the launcher.
func startReconcileCoordinator(ctx context.Context, wg *sync.WaitGroup, w *output.Writer, c *config, p profile.Profile) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		var trigger <-chan time.Time
		var syncCh <-chan struct{}
		switch c.mode {
		case "drift":
			period := p.Reconcile.Period
			if period <= 0 {
				return
			}
			offset := time.Duration(rand.Int63n(int64(period)))
			select {
			case <-ctx.Done():
				return
			case <-time.After(offset):
			}
			tk := time.NewTicker(period)
			defer tk.Stop()
			trigger = tk.C
		case "sync":
			ch, err := chaossync.Connect(ctx, c.syncSocket)
			if err != nil {
				w.Emit(map[string]any{
					"ts":    now(),
					"type":  "sync_error",
					"error": err.Error(),
				})
				// Fall back to drift to keep the worker alive.
				period := p.Reconcile.Period
				if period <= 0 {
					return
				}
				tk := time.NewTicker(period)
				defer tk.Stop()
				trigger = tk.C
			} else {
				syncCh = ch
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-trigger:
				runReconcileBurst(ctx, w, p)
			case _, ok := <-syncCh:
				if !ok {
					return
				}
				runReconcileBurst(ctx, w, p)
			}
		}
	}()
}

// runReconcileBurst runs one burst window: emit start, spawn the spike
// goroutines doing burst-multiplied work for the burst duration, emit
// end. Burst goroutines are short-lived; they exit when the burst
// window closes.
func runReconcileBurst(parentCtx context.Context, w *output.Writer, p profile.Profile) {
	if p.Reconcile.Duration <= 0 {
		return
	}
	w.Emit(map[string]any{"ts": now(), "type": "reconcile_start"})
	burstCtx, cancel := context.WithTimeout(parentCtx, p.Reconcile.Duration)
	defer cancel()

	var wg sync.WaitGroup
	// Goroutine spike: spawn N burst goroutines doing alloc work at
	// the multiplied rate.
	if p.Reconcile.GoroutineSpike > 0 && p.Reconcile.AllocMultiplier > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workload.RunAlloc(burstCtx, workload.AllocConfig{
				Goroutines:  p.Reconcile.GoroutineSpike,
				BytesPerSec: int64(float64(p.SteadyState.AllocBytesPerSec) * p.Reconcile.AllocMultiplier),
				SizeDist:    p.SteadyState.AllocSizeDist,
			})
		}()
	}
	// Syscall multiplier: extra burst syscalls.
	if p.Reconcile.SyscallMultiplier > 1 && p.SteadyState.SyscallTarget != workload.SyscallNone {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = workload.RunSyscall(burstCtx, workload.SyscallConfig{
				Target:         p.SteadyState.SyscallTarget,
				Goroutines:     max1(p.Reconcile.GoroutineSpike / 4),
				SyscallsPerSec: int(float64(p.SteadyState.SyscallsPerSec) * (p.Reconcile.SyscallMultiplier - 1)),
			})
		}()
	}
	wg.Wait()
	w.Emit(map[string]any{"ts": now(), "type": "reconcile_end"})
}
