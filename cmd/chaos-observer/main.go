// Command chaos-observer samples /proc at a configurable interval
// (default 1 Hz) for the duration of a run.
//
// Each sample captures aggregate worker thread count and RSS, system
// load average, runqueue depth, context-switch rate, and softirq /
// IRQ / steal time deltas. Output is a CSV at
// <output-dir>/observer.csv.
//
// See ../../README.md and ../../../chaos-harness-design.md §6.
package main

import (
	"bufio"
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = "1.1.0"

type config struct {
	outputDir       string
	runID           string
	duration        time.Duration
	sampleInterval  time.Duration
	workerPIDFilter string
	showVersion     bool
}

func parseFlags() (*config, error) {
	c := &config{}
	flag.StringVar(&c.outputDir, "output-dir", "", "directory for CSV output")
	flag.StringVar(&c.runID, "run-id", "", "run identifier; embedded in output filenames")
	flag.DurationVar(&c.duration, "duration", 600*time.Second, "run duration before clean exit")
	flag.DurationVar(&c.sampleInterval, "sample-interval", time.Second, "sample period")
	flag.StringVar(&c.workerPIDFilter, "worker-pid-filter", "chaos-worker", "comm/cmdline substring to identify worker PIDs")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()
	if c.showVersion {
		return c, nil
	}
	if c.outputDir == "" {
		return nil, fmt.Errorf("--output-dir is required")
	}
	if c.runID == "" {
		return nil, fmt.Errorf("--run-id is required")
	}
	return c, nil
}

func main() {
	c, err := parseFlags()
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-observer:", err)
		flag.Usage()
		os.Exit(2)
	}
	if c.showVersion {
		fmt.Println("chaos-observer", version)
		return
	}
	if err := os.MkdirAll(c.outputDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "chaos-observer:", err)
		os.Exit(1)
	}
	csvPath := filepath.Join(c.outputDir, "observer.csv")
	f, err := os.Create(csvPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "chaos-observer:", err)
		os.Exit(1)
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	_ = w.Write([]string{
		"ts", "worker_count", "total_threads", "total_rss_mb",
		"loadavg_1", "procs_running",
		"ctxt_per_sec", "softirq_pct", "irq_pct", "steal_pct", "user_pct", "system_pct", "iowait_pct", "idle_pct",
	})
	w.Flush()

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

	t := time.NewTicker(c.sampleInterval)
	defer t.Stop()
	prev, _ := readStat()
	for {
		select {
		case <-ctx.Done():
			return
		case ts := <-t.C:
			cur, err := readStat()
			if err != nil {
				continue
			}
			d := cur.delta(prev)
			prev = cur
			workerPIDs := findWorkerPIDs(c.workerPIDFilter)
			threads, rssBytes := sumThreadsAndRSS(workerPIDs)
			loadavg := readLoadavg1()
			procsRunning := readProcsRunning()

			_ = w.Write([]string{
				strconv.FormatFloat(float64(ts.UnixNano())/1e9, 'f', 3, 64),
				strconv.Itoa(len(workerPIDs)),
				strconv.Itoa(threads),
				strconv.FormatFloat(float64(rssBytes)/1024/1024, 'f', 1, 64),
				strconv.FormatFloat(loadavg, 'f', 2, 64),
				strconv.Itoa(procsRunning),
				strconv.FormatInt(d.ctxt, 10),
				strconv.FormatFloat(d.softirqPct, 'f', 2, 64),
				strconv.FormatFloat(d.irqPct, 'f', 2, 64),
				strconv.FormatFloat(d.stealPct, 'f', 2, 64),
				strconv.FormatFloat(d.userPct, 'f', 2, 64),
				strconv.FormatFloat(d.systemPct, 'f', 2, 64),
				strconv.FormatFloat(d.iowaitPct, 'f', 2, 64),
				strconv.FormatFloat(d.idlePct, 'f', 2, 64),
			})
			w.Flush()
		}
	}
}

// statSample captures the totals from /proc/stat needed for delta
// computation. Fields beyond idle (iowait, irq, softirq, steal,
// guest, guest_nice) follow the kernel's documented order.
type statSample struct {
	ts                                                       time.Time
	user, nice, system, idle, iowait, irq, softirq, steal    uint64
	ctxt                                                     uint64
}

func readStat() (statSample, error) {
	var s statSample
	f, err := os.Open("/proc/stat")
	if err != nil {
		return s, err
	}
	defer f.Close()
	s.ts = time.Now()
	br := bufio.NewScanner(f)
	for br.Scan() {
		line := br.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			vals := make([]uint64, 0, 8)
			for _, v := range fields[1:] {
				n, _ := strconv.ParseUint(v, 10, 64)
				vals = append(vals, n)
			}
			get := func(i int) uint64 {
				if i >= len(vals) {
					return 0
				}
				return vals[i]
			}
			s.user = get(0)
			s.nice = get(1)
			s.system = get(2)
			s.idle = get(3)
			s.iowait = get(4)
			s.irq = get(5)
			s.softirq = get(6)
			s.steal = get(7)
		} else if strings.HasPrefix(line, "ctxt ") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				s.ctxt, _ = strconv.ParseUint(fields[1], 10, 64)
			}
		}
	}
	return s, nil
}

type delta struct {
	ctxt                                                            int64
	userPct, systemPct, idlePct, iowaitPct, irqPct, softirqPct, stealPct float64
}

func (s statSample) delta(prev statSample) delta {
	d := delta{}
	if prev.ts.IsZero() {
		return d
	}
	dt := s.ts.Sub(prev.ts).Seconds()
	if dt <= 0 {
		return d
	}
	d.ctxt = int64((s.ctxt - prev.ctxt))
	if dt > 0 {
		d.ctxt = int64(float64(d.ctxt) / dt)
	}
	totalPrev := prev.user + prev.nice + prev.system + prev.idle + prev.iowait + prev.irq + prev.softirq + prev.steal
	totalNow := s.user + s.nice + s.system + s.idle + s.iowait + s.irq + s.softirq + s.steal
	totalDelta := float64(totalNow - totalPrev)
	if totalDelta <= 0 {
		return d
	}
	pct := func(now, prevV uint64) float64 {
		return 100 * float64(now-prevV) / totalDelta
	}
	d.userPct = pct(s.user+s.nice, prev.user+prev.nice)
	d.systemPct = pct(s.system, prev.system)
	d.idlePct = pct(s.idle, prev.idle)
	d.iowaitPct = pct(s.iowait, prev.iowait)
	d.irqPct = pct(s.irq, prev.irq)
	d.softirqPct = pct(s.softirq, prev.softirq)
	d.stealPct = pct(s.steal, prev.steal)
	return d
}

// findWorkerPIDs scans /proc and returns PIDs whose comm or argv[0]
// contains filter. The filter is intentionally permissive — comm is
// truncated to 15 chars on Linux so an exact match would miss
// "chaos-worker".
func findWorkerPIDs(filter string) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	out := make([]int, 0, 64)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		comm, _ := os.ReadFile("/proc/" + e.Name() + "/comm")
		if strings.Contains(string(comm), filter) {
			out = append(out, pid)
			continue
		}
		// Fallback: cmdline may have the full path.
		if cmdline, err := os.ReadFile("/proc/" + e.Name() + "/cmdline"); err == nil {
			if strings.Contains(string(cmdline), filter) {
				out = append(out, pid)
			}
		}
	}
	return out
}

func sumThreadsAndRSS(pids []int) (threads int, rssBytes int64) {
	for _, pid := range pids {
		f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		s := bufio.NewScanner(f)
		for s.Scan() {
			line := s.Text()
			switch {
			case strings.HasPrefix(line, "Threads:"):
				fields := strings.Fields(line)
				if len(fields) == 2 {
					n, _ := strconv.Atoi(fields[1])
					threads += n
				}
			case strings.HasPrefix(line, "VmRSS:"):
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					n, _ := strconv.ParseInt(fields[1], 10, 64)
					rssBytes += n * 1024
				}
			}
		}
		f.Close()
	}
	return
}

func readLoadavg1() float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fields[0], 64)
	return v
}

func readProcsRunning() int {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "procs_running") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				n, _ := strconv.Atoi(fields[1])
				return n
			}
		}
	}
	return 0
}
