package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "operator-generic",
		Description: "Generic platform operator — fleet-quantity profile, mostly idle informer + worker queue",
		SteadyState: SteadyConfig{
			Goroutines:       50,
			AllocBytesPerSec: 1 * 1024 * 1024, // 1 MB/s
			AllocSizeDist:    workload.DefaultSizeDist(),
			ChannelOpsPerSec: 5_000,
			SyscallTarget:    workload.SyscallLoopbackTCP,
			SyscallsPerSec:   50,
			LockContention:   workload.ContentionLow,
			LockOpsPerSec:    100,
		},
		Reconcile: ReconcileConfig{
			Period:            30 * time.Second,
			Duration:          100 * time.Millisecond,
			AllocMultiplier:   2,
			GoroutineSpike:    100,
			SyscallMultiplier: 1,
		},
	})
}
