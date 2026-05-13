// Command chaos-victim measures node-level scheduler interference.
//
// Two modes:
//
//	hires_jitter (default): a 1 kHz clock_nanosleep CLOCK_MONOTONIC
//	  TIMER_ABSTIME loop. Each iteration computes target = previous +
//	  1 ms, sleeps until target, and records (actual - target) wakeup
//	  jitter in nanoseconds into an HDR histogram. p99.9 is the
//	  headline metric.
//
//	http_rtt: a localhost HTTP server on a free port plus a client
//	  loop issuing 100 RPS small requests, recording end-to-end
//	  round-trip latency.
//
// Output: <output-dir>/victim.hdr (HDR histogram serialised),
// <output-dir>/victim-buckets.csv (per-second p50/p95/p99/p999 in µs).
//
// See ../../README.md and ../../../chaos-harness-design.md §5.
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
)

var version = "1.1.0"

type config struct {
	mode        string
	pinningCPUs string
	outputDir   string
	runID       string
	duration    time.Duration
	httpAddr    string
	rps         int
	showVersion bool
}

func parseFlags() (*config, error) {
	c := &config{}
	flag.StringVar(&c.mode, "mode", "hires_jitter", "victim mode: hires_jitter|http_rtt")
	flag.StringVar(&c.pinningCPUs, "pinning-cpus", "", "optional CPU set for sched_setaffinity (e.g. 0-3); empty = unconstrained")
	flag.StringVar(&c.outputDir, "output-dir", "", "directory for HDR histogram + bucket CSV output")
	flag.StringVar(&c.runID, "run-id", "", "run identifier; embedded in output filenames")
	flag.DurationVar(&c.duration, "duration", 600*time.Second, "run duration before clean exit")
	flag.StringVar(&c.httpAddr, "http-addr", "127.0.0.1:0", "http_rtt mode: server bind address (port 0 picks free)")
	flag.IntVar(&c.rps, "rps", 100, "http_rtt mode: client request rate")
	flag.BoolVar(&c.showVersion, "version", false, "print version and exit")
	flag.Parse()
	if c.showVersion {
		return c, nil
	}
	if c.mode != "hires_jitter" && c.mode != "http_rtt" {
		return nil, fmt.Errorf("--mode must be hires_jitter or http_rtt, got %q", c.mode)
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
		fmt.Fprintln(os.Stderr, "chaos-victim:", err)
		flag.Usage()
		os.Exit(2)
	}
	if c.showVersion {
		fmt.Println("chaos-victim", version)
		return
	}
	if err := os.MkdirAll(c.outputDir, 0o755); err != nil {
		fatal(err)
	}
	if c.pinningCPUs != "" {
		if err := pinSelf(c.pinningCPUs); err != nil {
			fmt.Fprintln(os.Stderr, "chaos-victim: pin:", err)
			// Continue rather than fail — pinning is informational.
		}
	}

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

	switch c.mode {
	case "hires_jitter":
		runJitter(ctx, c)
	case "http_rtt":
		runHTTP(ctx, c)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "chaos-victim:", err)
	os.Exit(1)
}

// runJitter implements the 1 kHz clock_nanosleep loop.
func runJitter(ctx context.Context, c *config) {
	// HDR: 1ns to 10s, 3 sig figs. Generous range so we don't lose
	// precision when the chaos workers blow latency up.
	h := hdr.New(1, int64(10*time.Second), 3)

	bucketsCSV, err := os.Create(filepath.Join(c.outputDir, "victim-buckets.csv"))
	if err != nil {
		fatal(err)
	}
	defer bucketsCSV.Close()
	w := csv.NewWriter(bucketsCSV)
	_ = w.Write([]string{"ts", "p50_us", "p95_us", "p99_us", "p999_us", "max_us", "count"})

	const period = time.Millisecond
	target := time.Now().Add(period)

	var bucketMu sync.Mutex
	bucketHist := hdr.New(1, int64(10*time.Second), 3)
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ts := <-t.C:
				bucketMu.Lock()
				p50 := bucketHist.ValueAtQuantile(50)
				p95 := bucketHist.ValueAtQuantile(95)
				p99 := bucketHist.ValueAtQuantile(99)
				p999 := bucketHist.ValueAtQuantile(99.9)
				maxV := bucketHist.Max()
				count := bucketHist.TotalCount()
				bucketHist.Reset()
				bucketMu.Unlock()
				_ = w.Write([]string{
					strconv.FormatFloat(float64(ts.UnixNano())/1e9, 'f', 3, 64),
					strconv.FormatInt(p50/1000, 10),
					strconv.FormatInt(p95/1000, 10),
					strconv.FormatInt(p99/1000, 10),
					strconv.FormatInt(p999/1000, 10),
					strconv.FormatInt(maxV/1000, 10),
					strconv.FormatInt(count, 10),
				})
				w.Flush()
			}
		}
	}()

	for {
		if ctx.Err() != nil {
			break
		}
		if err := sleepUntil(target); err != nil {
			// Cancelled or interrupted; loop will exit on ctx check.
			if ctx.Err() != nil {
				break
			}
		}
		actual := time.Now()
		jitter := actual.Sub(target).Nanoseconds()
		if jitter < 0 {
			jitter = 0
		}
		_ = h.RecordValue(jitter)
		bucketMu.Lock()
		_ = bucketHist.RecordValue(jitter)
		bucketMu.Unlock()
		target = target.Add(period)
		// If we fall far behind (e.g. paused process), skip ahead so
		// we don't burn CPU catching up.
		if behind := actual.Sub(target); behind > 100*time.Millisecond {
			target = actual.Add(period)
		}
	}

	// Persist HDR histogram. The hdrhistogram-go library does not
	// ship a built-in serialiser, so we write a simple two-column
	// "value_ns count" snapshot of the histogram bars.
	hdrPath := filepath.Join(c.outputDir, "victim.hdr")
	if err := writeHDRSnapshot(hdrPath, h); err != nil {
		fmt.Fprintln(os.Stderr, "chaos-victim: write hdr:", err)
	}
	fmt.Printf("chaos-victim: hires_jitter done. count=%d p50=%dµs p95=%dµs p99=%dµs p99.9=%dµs max=%dµs\n",
		h.TotalCount(),
		h.ValueAtQuantile(50)/1000,
		h.ValueAtQuantile(95)/1000,
		h.ValueAtQuantile(99)/1000,
		h.ValueAtQuantile(99.9)/1000,
		h.Max()/1000,
	)
}

// writeHDRSnapshot dumps non-zero bars from h to a 2-column TSV-ish
// file readable by aggregate-results.py without needing the upstream
// HDR-format parser.
func writeHDRSnapshot(path string, h *hdr.Histogram) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, _ = fmt.Fprintln(f, "# HDR snapshot — value_ns count")
	for _, b := range h.Distribution() {
		if b.Count == 0 {
			continue
		}
		_, _ = fmt.Fprintf(f, "%d\t%d\n", b.To, b.Count)
	}
	return nil
}

func runHTTP(ctx context.Context, c *config) {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte("ok"))
		}),
	}
	ln, err := net.Listen("tcp", c.httpAddr)
	if err != nil {
		fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	addr := "http://" + ln.Addr().String() + "/"

	h := hdr.New(1, int64(10*time.Second), 3)
	bucketsCSV, err := os.Create(filepath.Join(c.outputDir, "victim-buckets.csv"))
	if err != nil {
		fatal(err)
	}
	defer bucketsCSV.Close()
	wcsv := csv.NewWriter(bucketsCSV)
	_ = wcsv.Write([]string{"ts", "p50_us", "p95_us", "p99_us", "p999_us", "max_us", "count"})

	interval := time.Second / time.Duration(c.rps)
	t := time.NewTicker(interval)
	defer t.Stop()
	bucketHist := hdr.New(1, int64(10*time.Second), 3)
	var bucketMu sync.Mutex
	go func() {
		secT := time.NewTicker(time.Second)
		defer secT.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case ts := <-secT.C:
				bucketMu.Lock()
				_ = wcsv.Write([]string{
					strconv.FormatFloat(float64(ts.UnixNano())/1e9, 'f', 3, 64),
					strconv.FormatInt(bucketHist.ValueAtQuantile(50)/1000, 10),
					strconv.FormatInt(bucketHist.ValueAtQuantile(95)/1000, 10),
					strconv.FormatInt(bucketHist.ValueAtQuantile(99)/1000, 10),
					strconv.FormatInt(bucketHist.ValueAtQuantile(99.9)/1000, 10),
					strconv.FormatInt(bucketHist.Max()/1000, 10),
					strconv.FormatInt(bucketHist.TotalCount(), 10),
				})
				bucketHist.Reset()
				bucketMu.Unlock()
				wcsv.Flush()
			}
		}
	}()

	client := &http.Client{Timeout: 5 * time.Second}
	for {
		select {
		case <-ctx.Done():
			_ = writeHDRSnapshot(filepath.Join(c.outputDir, "victim.hdr"), h)
			return
		case <-t.C:
			start := time.Now()
			resp, err := client.Get(addr)
			if err != nil {
				continue
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			rtt := time.Since(start).Nanoseconds()
			_ = h.RecordValue(rtt)
			bucketMu.Lock()
			_ = bucketHist.RecordValue(rtt)
			bucketMu.Unlock()
		}
	}
}

// parseCPUSet expands "0-3,7,9-11" into a slice of ints.
func parseCPUSet(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if i := strings.Index(part, "-"); i > 0 {
			lo, err1 := strconv.Atoi(part[:i])
			hi, err2 := strconv.Atoi(part[i+1:])
			if err1 != nil || err2 != nil || lo > hi {
				return nil, fmt.Errorf("bad CPU range %q", part)
			}
			for c := lo; c <= hi; c++ {
				out = append(out, c)
			}
			continue
		}
		c, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad CPU id %q", part)
		}
		out = append(out, c)
	}
	return out, nil
}
