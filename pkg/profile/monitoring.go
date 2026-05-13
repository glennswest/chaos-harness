package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "monitoring",
		Description: "Prometheus / Thanos — periodic-large-burst contributor",
		SteadyState: SteadyConfig{
			Goroutines:       100,
			AllocBytesPerSec: 5 * 1024 * 1024, // 5 MB/s
			AllocSizeDist:    workload.DefaultSizeDist(),
			ChannelOpsPerSec: 20_000,
			SyscallTarget:    workload.SyscallLoopbackTCP,
			SyscallsPerSec:   200,
			LockContention:   workload.ContentionModerate,
			LockOpsPerSec:    500,
		},
		Reconcile: ReconcileConfig{
			Period:            30 * time.Second,
			Duration:          500 * time.Millisecond,
			AllocMultiplier:   10,
			GoroutineSpike:    100,
			SyscallMultiplier: 1,
		},
	})
}
