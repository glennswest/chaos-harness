package workload

import (
	"context"
	"math/rand"
	"sync/atomic"
	"time"
)

// SizeDist is a discrete probability distribution over allocation sizes.
//
// Sizes and Weights have the same length. Weights need not sum to 1;
// they are normalised at sample time.
type SizeDist struct {
	Sizes   []int
	Weights []float64
}

// DefaultSizeDist is a 256B / 4KB / 64KB mix approximating typical Go
// service allocation patterns: lots of small request/response objects,
// some medium buffers, occasional large blobs.
func DefaultSizeDist() SizeDist {
	return SizeDist{
		Sizes:   []int{256, 4096, 65536},
		Weights: []float64{0.7, 0.25, 0.05},
	}
}

func (d SizeDist) sample(r *rand.Rand) int {
	if len(d.Sizes) == 0 {
		return 256
	}
	var total float64
	for _, w := range d.Weights {
		total += w
	}
	if total <= 0 {
		return d.Sizes[0]
	}
	pick := r.Float64() * total
	var cum float64
	for i, w := range d.Weights {
		cum += w
		if pick <= cum {
			return d.Sizes[i]
		}
	}
	return d.Sizes[len(d.Sizes)-1]
}

// AllocConfig parameterises the heap-allocation primitive.
//
// Goroutines independently allocate at a per-goroutine rate equal to
// BytesPerSec / Goroutines. Sinks holds a small ring of recent
// allocations to defeat the optimiser without producing unbounded heap
// growth — old entries are overwritten and become garbage.
type AllocConfig struct {
	Goroutines   int
	BytesPerSec  int64
	SizeDist     SizeDist
	RingSize     int // per-goroutine retained allocations; default 64
}

// RunAlloc runs the allocation primitive until ctx is done.
func RunAlloc(ctx context.Context, cfg AllocConfig) {
	if cfg.Goroutines <= 0 || cfg.BytesPerSec <= 0 {
		return
	}
	if cfg.RingSize <= 0 {
		cfg.RingSize = 64
	}
	if len(cfg.SizeDist.Sizes) == 0 {
		cfg.SizeDist = DefaultSizeDist()
	}
	perGoroutineBPS := cfg.BytesPerSec / int64(cfg.Goroutines)
	if perGoroutineBPS < 1 {
		perGoroutineBPS = 1
	}
	done := make(chan struct{})
	var live atomic.Int32
	live.Store(int32(cfg.Goroutines))
	for i := 0; i < cfg.Goroutines; i++ {
		seed := int64(i) ^ time.Now().UnixNano()
		go func(seed int64) {
			defer func() {
				if live.Add(-1) == 0 {
					close(done)
				}
			}()
			r := rand.New(rand.NewSource(seed))
			ring := make([][]byte, cfg.RingSize)
			ringIdx := 0
			// Drip allocations in small bursts every interval so the
			// scheduler doesn't spike. The interval is sized to roughly
			// 1 ms between bursts; burst size scales with rate.
			const burstInterval = time.Millisecond
			burstBytes := int64(perGoroutineBPS) * int64(burstInterval) / int64(time.Second)
			if burstBytes < 1 {
				burstBytes = 1
			}
			ticker := time.NewTicker(burstInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					var allocated int64
					for allocated < burstBytes {
						sz := cfg.SizeDist.sample(r)
						buf := make([]byte, sz)
						// Touch first byte so the kernel actually
						// commits the page.
						buf[0] = byte(sz)
						ring[ringIdx] = buf
						ringIdx = (ringIdx + 1) % cfg.RingSize
						allocated += int64(sz)
					}
				}
			}
		}(seed)
	}
	select {
	case <-ctx.Done():
	case <-done:
	}
}
