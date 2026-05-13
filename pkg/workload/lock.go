package workload

import (
	"context"
	"sync"
	"time"
)

// ContentionLevel selects mutex contention intensity.
type ContentionLevel string

const (
	ContentionNone     ContentionLevel = "none"
	ContentionLow      ContentionLevel = "low"
	ContentionModerate ContentionLevel = "moderate"
	ContentionHigh     ContentionLevel = "high"
)

// LockConfig parameterises the contended-mutex primitive.
type LockConfig struct {
	Level      ContentionLevel
	Goroutines int
	OpsPerSec  int
}

// RunLock runs the contention primitive until ctx is done.
//
// All goroutines share a single mutex and an integer counter. Each
// goroutine acquires, increments, and releases at a pace dictated by
// OpsPerSec / Goroutines. CritSection holds the lock briefly to
// produce realistic wait-then-wake patterns.
func RunLock(ctx context.Context, cfg LockConfig) {
	if cfg.Level == ContentionNone || cfg.Level == "" {
		return
	}
	if cfg.Goroutines <= 0 || cfg.OpsPerSec <= 0 {
		return
	}
	var mu sync.Mutex
	var counter int64
	// Critical-section duration scales with contention level. These
	// are intentionally small (sub-microsecond at the low end) so the
	// primitive doesn't become a CPU sink — the goal is wakeups and
	// futex traffic, not held-time CPU burn.
	var critWork int
	switch cfg.Level {
	case ContentionLow:
		critWork = 8
	case ContentionModerate:
		critWork = 64
	case ContentionHigh:
		critWork = 512
	default:
		critWork = 16
	}
	perGoroutine := cfg.OpsPerSec / cfg.Goroutines
	if perGoroutine < 1 {
		perGoroutine = 1
	}
	interval := time.Second / time.Duration(perGoroutine)

	var wg sync.WaitGroup
	wg.Add(cfg.Goroutines)
	for i := 0; i < cfg.Goroutines; i++ {
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					mu.Lock()
					counter++
					// Tiny computation in the critical section so
					// holds are non-zero but bounded.
					var x int
					for j := 0; j < critWork; j++ {
						x += j
					}
					_ = x
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()
}
