package tuning

import (
	"context"
	"fmt"
	"os/exec"
)

// Applier wraps an exec.Cmd so the spawned process inherits the CPU,
// memory, NUMA, and RT-priority constraints of an Assignment.
//
// Implementations:
//
//	systemdApplier  — `systemd-run --scope --slice=chaos.slice
//	                  --property=AllowedCPUs=...` (preferred; clean
//	                  lifecycle, cgroup v2 native).
//	cgroupApplier   — direct cgroup v2 writes under /sys/fs/cgroup/chaos.slice/
//	                  with cpuset.cpus + cpuset.cpus.exclusive + memory.max.
//	tasksetApplier  — taskset+chrt+numactl pre-exec wrapping; memory limits
//	                  are unsupported and noted in the verify report.
//
// The Applier MUST NOT exec the wrapped command itself; it returns a
// modified *exec.Cmd that the caller hands to its own .Start()/.Wait()
// machinery. This keeps cleanup, stdio, and signal handling unchanged.
type Applier interface {
	// Name returns a short identifier ("systemd-run", "cgroup-v2", "taskset").
	Name() string

	// Available reports whether this applier can run on the current host.
	// It must be cheap (no side effects); callers use it to pick a default.
	Available(ctx context.Context) bool

	// Wrap returns a new *exec.Cmd that, when started, will be confined to
	// the assignment. Implementations that prepend a wrapper command
	// (systemd-run, taskset, ...) will replace cmd.Path and cmd.Args;
	// the original inner program is preserved. The returned cmd is
	// bound to ctx via exec.CommandContext, so callers should pass the
	// same context they'd use for spawning the unwrapped program.
	Wrap(ctx context.Context, cmd *exec.Cmd, a Assignment) (*exec.Cmd, error)

	// PrepareHost performs one-time host setup needed by this backend
	// (e.g. cgroup-v2 creates /sys/fs/cgroup/chaos.slice/ subdirs;
	// systemd-run is a no-op). Should be idempotent.
	PrepareHost(ctx context.Context, plan *Plan) error

	// CleanupHost reverses PrepareHost. Idempotent.
	CleanupHost(ctx context.Context) error
}

// AutoSelect returns the first available applier from the preferred order:
// systemd → cgroup v2 → taskset. Returns an error only if NONE work,
// which on RHEL is essentially impossible.
//
// availabilityOverride lets callers force a specific backend by name
// (matching .Name()); empty string = autoselect.
func AutoSelect(ctx context.Context, availabilityOverride string) (Applier, error) {
	candidates := []Applier{
		NewSystemdApplier(),
		NewCgroupApplier(),
		NewTasksetApplier(),
	}
	if availabilityOverride != "" {
		for _, c := range candidates {
			if c.Name() == availabilityOverride {
				if !c.Available(ctx) {
					return nil, fmt.Errorf("requested applier %q is not available on this host", availabilityOverride)
				}
				return c, nil
			}
		}
		return nil, fmt.Errorf("unknown applier %q; valid: systemd-run, cgroup-v2, taskset", availabilityOverride)
	}
	for _, c := range candidates {
		if c.Available(ctx) {
			return c, nil
		}
	}
	return nil, fmt.Errorf("no tuning applier available (no systemd-run, no cgroup v2 write, no taskset)")
}
