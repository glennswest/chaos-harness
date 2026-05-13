package tuning

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
)

// systemdApplier wraps each spawned process in a transient systemd scope:
//
//	systemd-run --user=NO --scope --quiet --slice=chaos.slice
//	  --unit=chaos-<component>-<replica>
//	  --property=AllowedCPUs=4-7
//	  --property=MemoryMax=2G
//	  -- /path/to/chaos-worker --profile=...
//
// On cgroup v2 hosts (RHEL 9, RHEL 10), AllowedCPUs maps
// directly to cpuset.cpus on the scope's cgroup. The slice itself is
// created on first use; we do nothing in PrepareHost because systemd
// handles slice lifecycle implicitly.
type systemdApplier struct {
	slice string
}

// NewSystemdApplier constructs an Applier that uses systemd-run scopes
// under chaos.slice.
func NewSystemdApplier() Applier {
	return &systemdApplier{slice: "chaos.slice"}
}

func (s *systemdApplier) Name() string { return "systemd-run" }

func (s *systemdApplier) Available(ctx context.Context) bool {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	// systemd-run exists; smoke-test that it can spawn under a slice.
	// We can't actually run a transient scope here without side effects,
	// so trust the binary's presence.
	return true
}

func (s *systemdApplier) PrepareHost(ctx context.Context, plan *Plan) error {
	// Slices are created lazily by systemd-run. Nothing to do.
	return nil
}

func (s *systemdApplier) CleanupHost(ctx context.Context) error {
	// Stop any leftover transient scopes under chaos.slice. Best-effort —
	// if systemctl isn't available or no scopes exist, we don't error.
	if _, err := exec.LookPath("systemctl"); err != nil {
		return nil
	}
	cmd := exec.CommandContext(ctx, "systemctl", "stop", s.slice)
	_ = cmd.Run() // ignore error; slice may not exist
	return nil
}

func (s *systemdApplier) Wrap(ctx context.Context, cmd *exec.Cmd, a Assignment) (*exec.Cmd, error) {
	if cmd.Path == "" || len(cmd.Args) == 0 {
		return nil, fmt.Errorf("systemd applier: cmd has no Path/Args")
	}
	args := []string{
		"--scope",
		"--quiet",
		"--collect", // remove unit on exit
		"--slice=" + s.slice,
		"--unit=" + scopeUnitName(a),
	}
	if a.CPUs.Len() > 0 {
		args = append(args, "--property=AllowedCPUs="+a.CPUs.String())
	}
	if a.MemoryMax > 0 {
		args = append(args, "--property=MemoryMax="+strconv.FormatInt(a.MemoryMax, 10))
	}
	args = append(args, "--")
	// systemd-run on systemd <= 252 (RHEL 9.6) rejects
	// --property=CPUSchedulingPolicy=fifo on transient *scope* units;
	// that property is only honoured on service units. Instead, wrap
	// the inner cmd with `chrt -f N` so the spawned process calls
	// sched_setattr(SCHED_FIFO, prio=N) on itself before exec'ing the
	// real program. This works on every backend uniformly.
	if a.RTPriority > 0 {
		args = append(args, "chrt", "-f", strconv.Itoa(a.RTPriority))
	}
	args = append(args, cmd.Path)
	args = append(args, cmd.Args[1:]...)

	wrapped := exec.CommandContext(ctx, "systemd-run", args...)
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

func scopeUnitName(a Assignment) string {
	if a.ReplicaID != "" {
		return fmt.Sprintf("chaos-%s-%s.scope", sanitiseUnit(a.Component), sanitiseUnit(a.ReplicaID))
	}
	return fmt.Sprintf("chaos-%s.scope", sanitiseUnit(a.Component))
}

func sanitiseUnit(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
			out = append(out, c)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
