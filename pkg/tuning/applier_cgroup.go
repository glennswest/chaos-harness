package tuning

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// cgroupApplier writes directly to /sys/fs/cgroup/chaos.slice/<unit>/...
// without going through systemd. This is the fallback for systems where
// systemd-run is unavailable but cgroup v2 is mounted (which is standard
// on RHEL 9+).
//
// Per-process flow:
//
//  1. PrepareHost creates /sys/fs/cgroup/chaos.slice/ and writes
//     cgroup.subtree_control with "+cpuset +cpu +memory".
//  2. Wrap creates /sys/fs/cgroup/chaos.slice/<unit>/, writes cpuset.cpus,
//     cpuset.cpus.exclusive, memory.max.
//  3. Wrap returns a cmd whose first line is "echo $$ > .../cgroup.procs;
//     exec inner-cmd". We accomplish this by replacing the cmd with a
//     `sh -c` wrapper that joins the cgroup before exec'ing the inner.
//
// Because the move-into-cgroup happens in the spawned shell, the inner
// program (chaos-worker) starts with cgroup constraints already applied —
// equivalent to systemd-run's behaviour from the inner's point of view.
type cgroupApplier struct {
	root      string // typically /sys/fs/cgroup
	sliceName string // chaos.slice
}

// NewCgroupApplier returns an Applier that writes cgroup v2 controllers
// directly. Requires CAP_SYS_ADMIN or write access to /sys/fs/cgroup.
func NewCgroupApplier() Applier {
	return &cgroupApplier{root: "/sys/fs/cgroup", sliceName: "chaos.slice"}
}

func (c *cgroupApplier) Name() string { return "cgroup-v2" }

func (c *cgroupApplier) Available(ctx context.Context) bool {
	// Check that /sys/fs/cgroup is cgroup v2 (cgroup.controllers exists at root).
	if _, err := os.Stat(filepath.Join(c.root, "cgroup.controllers")); err != nil {
		return false
	}
	// Check that we can write to the root subtree_control.
	f, err := os.OpenFile(filepath.Join(c.root, "cgroup.subtree_control"), os.O_WRONLY, 0)
	if err != nil {
		return false
	}
	f.Close()
	// Check that `sh` exists (we use it to join cgroup before exec).
	if _, err := exec.LookPath("sh"); err != nil {
		return false
	}
	return true
}

func (c *cgroupApplier) PrepareHost(ctx context.Context, plan *Plan) error {
	slicePath := c.slicePath()
	if err := os.MkdirAll(slicePath, 0o755); err != nil {
		return fmt.Errorf("cgroup applier: mkdir %s: %w", slicePath, err)
	}
	// Enable controllers in chaos.slice's subtree.
	if err := os.WriteFile(
		filepath.Join(slicePath, "cgroup.subtree_control"),
		[]byte("+cpuset +cpu +memory"),
		0o644,
	); err != nil {
		// subtree_control may already have these; check that's the case.
		// Best-effort — if writing fails because controllers are already
		// enabled, the next write will succeed.
		if _, err2 := os.Stat(filepath.Join(slicePath, "cpuset.cpus")); err2 == nil {
			return nil // already configured
		}
		return fmt.Errorf("cgroup applier: enable controllers: %w", err)
	}
	return nil
}

func (c *cgroupApplier) CleanupHost(ctx context.Context) error {
	// Remove all per-unit dirs and the slice itself. Best-effort — a
	// busy cgroup will refuse to rmdir, which is fine; the next run
	// will reuse it.
	entries, err := os.ReadDir(c.slicePath())
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if e.IsDir() {
			_ = os.Remove(filepath.Join(c.slicePath(), e.Name()))
		}
	}
	_ = os.Remove(c.slicePath())
	return nil
}

func (c *cgroupApplier) slicePath() string {
	return filepath.Join(c.root, c.sliceName)
}

func (c *cgroupApplier) unitDir(a Assignment) string {
	return filepath.Join(c.slicePath(), scopeUnitName(a))
}

func (c *cgroupApplier) Wrap(ctx context.Context, cmd *exec.Cmd, a Assignment) (*exec.Cmd, error) {
	if cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, fmt.Errorf("cgroup applier: cmd has no Path/Args")
	}
	dir := c.unitDir(a)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("cgroup applier: mkdir %s: %w", dir, err)
	}
	if a.CPUs.Len() > 0 {
		if err := os.WriteFile(filepath.Join(dir, "cpuset.cpus"), []byte(a.CPUs.String()), 0o644); err != nil {
			return nil, fmt.Errorf("cgroup applier: write cpuset.cpus: %w", err)
		}
	}
	if a.Exclusive {
		// cpuset.cpus.exclusive is cgroup v2 (kernel 6.0+). Best-effort
		// — silently ignore on older kernels because cpuset.cpus alone
		// is sufficient for the spike-isolation invariant; exclusive is
		// belt-and-braces.
		_ = os.WriteFile(filepath.Join(dir, "cpuset.cpus.exclusive"), []byte(a.CPUs.String()), 0o644)
	}
	if a.MemoryMax > 0 {
		if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(strconv.FormatInt(a.MemoryMax, 10)), 0o644); err != nil {
			return nil, fmt.Errorf("cgroup applier: write memory.max: %w", err)
		}
	}

	procsFile := filepath.Join(dir, "cgroup.procs")
	innerArgs := append([]string{cmd.Path}, cmd.Args[1:]...)

	// Build a sh wrapper that:
	//   1. writes its own pid into cgroup.procs (joining the cgroup),
	//   2. optionally promotes itself to SCHED_FIFO via chrt,
	//   3. exec's the inner program (replacing the shell so signals pass through).
	//
	// shell-quote each arg to preserve any embedded spaces.
	prelude := fmt.Sprintf("printf %%d $$ > %s\n", shellQuote(procsFile))
	if a.RTPriority > 0 {
		prelude += fmt.Sprintf("exec chrt -f %d ", a.RTPriority)
	} else {
		prelude += "exec "
	}
	for _, arg := range innerArgs {
		prelude += shellQuote(arg) + " "
	}

	wrapped := exec.CommandContext(ctx, "sh", "-c", prelude)
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

// shellQuote single-quotes s for safe inclusion in a `sh -c` string.
// Any embedded ' is escaped as '\'' (close-quote, escaped quote, reopen).
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	out += "'"
	return out
}
