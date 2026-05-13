package tuning

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// tasksetApplier wraps each spawned process with `taskset -c <cpuset> [chrt
// -f <prio>] [numactl --cpunodebind=N --membind=N] -- <cmd>`. This is the
// last-resort fallback for hosts without systemd or cgroup v2 write access.
//
// Limitations vs the other backends:
//   - No cgroup memory limit (MemoryMax is ignored, with a warning).
//   - Exclusive cpusets are only enforced inside the chaos-harness — nothing
//     prevents another process on the host from hopping onto the same CPUs.
//     For spike-vs-victim isolation this is still sufficient because every
//     chaos process is wrapped, but on a noisy host it's weaker than systemd
//     scopes.
type tasksetApplier struct{}

// NewTasksetApplier returns the simplest possible Applier — pure pre-exec
// wrapping with classic Unix tools.
func NewTasksetApplier() Applier {
	return &tasksetApplier{}
}

func (t *tasksetApplier) Name() string { return "taskset" }

func (t *tasksetApplier) Available(ctx context.Context) bool {
	_, err := exec.LookPath("taskset")
	return err == nil
}

func (t *tasksetApplier) PrepareHost(ctx context.Context, plan *Plan) error { return nil }
func (t *tasksetApplier) CleanupHost(ctx context.Context) error             { return nil }

func (t *tasksetApplier) Wrap(ctx context.Context, cmd *exec.Cmd, a Assignment) (*exec.Cmd, error) {
	if cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, fmt.Errorf("taskset applier: cmd has no Path/Args")
	}

	var prefix []string

	// numactl wraps outside taskset so taskset's affinity wins.
	if a.NUMANode >= 0 {
		if _, err := exec.LookPath("numactl"); err == nil {
			node := strconv.Itoa(a.NUMANode)
			prefix = append(prefix, "numactl", "--cpunodebind="+node, "--membind="+node, "--")
		}
	}

	if a.CPUs.Len() > 0 {
		prefix = append(prefix, "taskset", "-c", a.CPUs.String())
	}

	if a.RTPriority > 0 {
		if _, err := exec.LookPath("chrt"); err == nil {
			prefix = append(prefix, "chrt", "-f", strconv.Itoa(a.RTPriority))
		} else {
			return nil, fmt.Errorf("taskset applier: chrt not available but rtPriority=%d requested", a.RTPriority)
		}
	}

	if a.MemoryMax > 0 {
		// taskset has no memory cap; we can't honor MemoryMax. Surface
		// this so callers can decide to enforce strict mode.
		return nil, fmt.Errorf("taskset applier: cannot honor memoryMaxBytes=%d (use systemd or cgroup backend)", a.MemoryMax)
	}

	prefix = append(prefix, cmd.Path)
	prefix = append(prefix, cmd.Args[1:]...)

	wrapped := exec.CommandContext(ctx, prefix[0], prefix[1:]...)
	wrapped.Env = cmd.Env
	if a.GOMAXPROCS > 0 {
		wrapped.Env = appendOrReplaceEnv(wrapped.Env, "GOMAXPROCS", strconv.Itoa(a.GOMAXPROCS))
	}
	wrapped.Stdout = cmd.Stdout
	wrapped.Stderr = cmd.Stderr
	wrapped.Stdin = cmd.Stdin
	wrapped.Dir = cmd.Dir
	wrapped.SysProcAttr = cmd.SysProcAttr
	return wrapped, nil
}
