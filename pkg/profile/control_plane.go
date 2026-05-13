package profile

import (
	"time"

	"github.com/glennswest/chaos-harness/pkg/workload"
)

func init() {
	register(Profile{
		Name:        "control-plane",
		Description: "kube-apiserver / controller-manager / scheduler — heavy fan-out, moderate alloc, low syscall",
		SteadyState: SteadyConfig{
			Goroutines:       200,
			AllocBytesPerSec: 10 * 1024 * 1024, // 10 MB/s
			AllocSizeDist: workload.SizeDist{
				Sizes:   []int{256, 4096},
				Weights: []float64{0.7, 0.3},
			},
			ChannelOpsPerSec: 50_000,
			SyscallTarget:    workload.SyscallLoopbackTCP,
			SyscallsPerSec:   100,
			LockContention:   workload.ContentionModerate,
			LockOpsPerSec:    1_000,
		},
		Reconcile: ReconcileConfig{
			Period:            30 * time.Second,
			Duration:          200 * time.Millisecond,
			AllocMultiplier:   5,
			GoroutineSpike:    500,
			SyscallMultiplier: 1,
		},
	})
}
