package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "networking",
		Description: "OVN-Kubernetes / multus / CNI plugins — highest M-explosion contributor",
		SteadyState: SteadyConfig{
			Goroutines:       80,
			AllocBytesPerSec: 3 * 1024 * 1024, // 3 MB/s
			AllocSizeDist:    workload.DefaultSizeDist(),
			ChannelOpsPerSec: 5_000,
			SyscallTarget:    workload.SyscallLoopbackTCP,
			SyscallsPerSec:   5_000,
			LockContention:   workload.ContentionLow,
			LockOpsPerSec:    200,
		},
		Reconcile: ReconcileConfig{
			Period:            15 * time.Second,
			Duration:          100 * time.Millisecond,
			AllocMultiplier:   1.5,
			GoroutineSpike:    200,
			SyscallMultiplier: 3,
		},
	})
}
