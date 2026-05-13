package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "logging",
		Description: "Vector / Loki — GC pressure dominator, high alloc churn",
		SteadyState: SteadyConfig{
			Goroutines:       150,
			AllocBytesPerSec: 50 * 1024 * 1024, // 50 MB/s
			AllocSizeDist: workload.SizeDist{
				Sizes:   []int{256, 4096, 16384},
				Weights: []float64{0.5, 0.4, 0.1},
			},
			ChannelOpsPerSec: 100_000,
			SyscallTarget:    workload.SyscallLoopbackTCP,
			SyscallsPerSec:   500,
			LockContention:   workload.ContentionModerate,
			LockOpsPerSec:    500,
		},
		Reconcile: ReconcileConfig{
			Period:            10 * time.Second,
			Duration:          200 * time.Millisecond,
			AllocMultiplier:   2,
			GoroutineSpike:    100,
			SyscallMultiplier: 1.5,
		},
	})
}
