package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "etcd-like",
		Description: "etcd-class store — fsync syscall + lock contention contributor",
		SteadyState: SteadyConfig{
			Goroutines:       100,
			AllocBytesPerSec: 2 * 1024 * 1024, // 2 MB/s
			AllocSizeDist:    workload.DefaultSizeDist(),
			ChannelOpsPerSec: 10_000,
			SyscallTarget:    workload.SyscallTmpfsFsync,
			SyscallsPerSec:   1_000,
			LockContention:   workload.ContentionHigh,
			LockOpsPerSec:    2_000,
		},
		Reconcile: ReconcileConfig{
			Period:            10 * time.Second,
			Duration:          50 * time.Millisecond,
			AllocMultiplier:   1.2,
			GoroutineSpike:    20,
			SyscallMultiplier: 2,
		},
	})
}
