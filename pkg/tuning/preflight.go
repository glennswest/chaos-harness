package tuning

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// PreflightCheck is one host-level expectation derived from a profile.
// In strict mode the launcher refuses to start if any required check
// fails. In best-effort mode (not yet exposed) failures become warnings.
type PreflightCheck struct {
	Name        string // short identifier, e.g. "isolcpus", "hugepages-1G"
	Required    bool   // true → strict mode aborts on failure
	Description string
	Pass        bool
	Detail      string // human-readable explanation of pass/fail
}

// PreflightReport collects all checks for a profile.
type PreflightReport struct {
	Checks []PreflightCheck
}

// AllPassed returns true if every check passed.
func (r PreflightReport) AllPassed() bool {
	for _, c := range r.Checks {
		if !c.Pass {
			return false
		}
	}
	return true
}

// RequiredFailures returns the subset of failed checks marked Required.
func (r PreflightReport) RequiredFailures() []PreflightCheck {
	var out []PreflightCheck
	for _, c := range r.Checks {
		if !c.Pass && c.Required {
			out = append(out, c)
		}
	}
	return out
}

// String renders a human-readable summary, one line per check.
func (r PreflightReport) String() string {
	var sb strings.Builder
	for _, c := range r.Checks {
		mark := "✓"
		if !c.Pass {
			if c.Required {
				mark = "✗"
			} else {
				mark = "!"
			}
		}
		fmt.Fprintf(&sb, "  %s %-20s %s\n", mark, c.Name, c.Detail)
	}
	return sb.String()
}

// Preflight performs all host-level expectations derived from p. Reads
// /proc/cmdline, /proc/meminfo, /sys/devices/system/cpu/.../cpufreq/, and
// the running irqbalance/systemd state. On non-Linux it returns an empty
// report (no checks apply).
func Preflight(p PerformanceProfile) PreflightReport {
	if runtime.GOOS != "linux" {
		return PreflightReport{Checks: []PreflightCheck{
			{Name: "platform", Required: false, Pass: false, Detail: "preflight is Linux-only; running on " + runtime.GOOS},
		}}
	}
	r := PreflightReport{}

	cmdline := readFile("/proc/cmdline")

	// isolcpus / nohz_full / rcu_nocbs should reference the isolated pool.
	isolated := p.IsolatedCPUs()
	for _, key := range []string{"isolcpus", "nohz_full", "rcu_nocbs"} {
		got := extractKernelArg(cmdline, key)
		if got == "" {
			r.Checks = append(r.Checks, PreflightCheck{
				Name:        key,
				Required:    p.RTEnabled(),
				Description: fmt.Sprintf("%s= must be set on kernel cmdline", key),
				Pass:        false,
				Detail:      fmt.Sprintf("not present in /proc/cmdline (expected %s=%s)", key, isolated),
			})
			continue
		}
		gotList, err := ParseCPUList(stripManagedSuffix(got))
		if err != nil {
			r.Checks = append(r.Checks, PreflightCheck{
				Name: key, Required: false, Pass: false,
				Detail: fmt.Sprintf("could not parse %q: %v", got, err),
			})
			continue
		}
		// We require isolated ⊆ gotList (the cmdline may isolate additional CPUs).
		if missing := isolated.Difference(gotList); missing.Len() > 0 {
			r.Checks = append(r.Checks, PreflightCheck{
				Name:        key,
				Required:    p.RTEnabled() && key == "isolcpus",
				Description: fmt.Sprintf("%s= must cover the isolated pool", key),
				Pass:        false,
				Detail:      fmt.Sprintf("cmdline %s=%s missing isolated CPUs %s", key, gotList, missing),
			})
			continue
		}
		r.Checks = append(r.Checks, PreflightCheck{
			Name: key, Required: false, Pass: true,
			Detail: fmt.Sprintf("cmdline %s=%s covers isolated %s", key, gotList, isolated),
		})
	}

	// Hugepages.
	for _, page := range p.Spec.Hugepages.Pages {
		// The kernel exposes per-size counts under
		// /sys/kernel/mm/hugepages/hugepages-<N>kB/nr_hugepages.
		var sizeKB int
		switch page.Size {
		case "1G":
			sizeKB = 1024 * 1024
		case "2M":
			sizeKB = 2048
		}
		path := fmt.Sprintf("/sys/kernel/mm/hugepages/hugepages-%dkB/nr_hugepages", sizeKB)
		got := strings.TrimSpace(readFile(path))
		want := page.Count
		if got == "" {
			r.Checks = append(r.Checks, PreflightCheck{
				Name:        "hugepages-" + page.Size,
				Required:    true,
				Description: fmt.Sprintf("expect %d hugepages of size %s", want, page.Size),
				Pass:        false,
				Detail:      fmt.Sprintf("%s not present (kernel may not support this size)", path),
			})
			continue
		}
		gotN, _ := strconv.Atoi(got)
		pass := gotN >= want
		r.Checks = append(r.Checks, PreflightCheck{
			Name:        "hugepages-" + page.Size,
			Required:    true,
			Description: fmt.Sprintf("expect %d hugepages of size %s", want, page.Size),
			Pass:        pass,
			Detail:      fmt.Sprintf("nr_hugepages=%d (want >= %d)", gotN, want),
		})
	}

	// CPU governor on isolated CPUs should be 'performance' if
	// highPowerConsumption is set.
	if p.Spec.WorkloadHints.HighPowerConsumption != nil && *p.Spec.WorkloadHints.HighPowerConsumption {
		bad := []int{}
		ok := []int{}
		for _, cpu := range p.IsolatedCPUs() {
			path := fmt.Sprintf("/sys/devices/system/cpu/cpu%d/cpufreq/scaling_governor", cpu)
			gov := strings.TrimSpace(readFile(path))
			if gov == "" {
				continue // no cpufreq subsystem; check skipped
			}
			if gov != "performance" {
				bad = append(bad, cpu)
			} else {
				ok = append(ok, cpu)
			}
		}
		var detail string
		if len(bad) > 0 {
			detail = fmt.Sprintf("%d/%d isolated CPUs not on performance governor (e.g. cpu%d)",
				len(bad), len(bad)+len(ok), bad[0])
		} else if len(ok) == 0 {
			detail = "no cpufreq subsystem detected (skipped)"
		} else {
			detail = fmt.Sprintf("all %d isolated CPUs on performance governor", len(ok))
		}
		r.Checks = append(r.Checks, PreflightCheck{
			Name:        "governor",
			Required:    true,
			Description: "highPowerConsumption requires performance governor on isolated CPUs",
			Pass:        len(bad) == 0,
			Detail:      detail,
		})
	}

	// RT kernel.
	if p.RTEnabled() {
		uname := strings.TrimSpace(readFile("/proc/sys/kernel/osrelease"))
		isRT := strings.Contains(uname, "rt") || strings.Contains(uname, "PREEMPT_RT")
		r.Checks = append(r.Checks, PreflightCheck{
			Name:        "realtime-kernel",
			Required:    true,
			Description: "spec.realTimeKernel.enabled requires kernel-rt",
			Pass:        isRT,
			Detail:      fmt.Sprintf("kernel = %q (want -rt)", uname),
		})
	}

	// IRQ load balancing globally disabled → irqbalance should not be running.
	if p.IRQLoadBalancingDisabled() {
		// Best-effort: check for a process named irqbalance via /proc.
		running := isProcessRunning("irqbalance")
		r.Checks = append(r.Checks, PreflightCheck{
			Name:        "irqbalance",
			Required:    false,
			Description: "globallyDisableIrqLoadBalancing expects irqbalance stopped (or banned-cpus set)",
			Pass:        !running,
			Detail: func() string {
				if running {
					return "irqbalance is running; consider 'systemctl stop irqbalance' or set IRQBALANCE_BANNED_CPULIST"
				}
				return "irqbalance not running"
			}(),
		})
	}

	return r
}

func readFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// extractKernelArg parses a /proc/cmdline string and returns the value of
// key=value (the part after the first =). Returns "" if not present.
// If the same key appears multiple times, the last wins (kernel behaviour).
func extractKernelArg(cmdline, key string) string {
	prefix := key + "="
	got := ""
	for _, tok := range strings.Fields(cmdline) {
		if strings.HasPrefix(tok, prefix) {
			got = strings.TrimPrefix(tok, prefix)
		}
	}
	return got
}

// stripManagedSuffix removes a leading "managed_irq," tag that
// PerformanceProfile-generated MachineConfigs add to isolcpus.
func stripManagedSuffix(s string) string {
	if i := strings.Index(s, ","); i >= 0 && (strings.HasPrefix(s, "managed_irq") || strings.HasPrefix(s, "domain")) {
		return s[i+1:]
	}
	return s
}

// isProcessRunning scans /proc for a process whose comm matches name.
func isProcessRunning(name string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		comm := strings.TrimSpace(readFile("/proc/" + e.Name() + "/comm"))
		if comm == name {
			return true
		}
	}
	return false
}
