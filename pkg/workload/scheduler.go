package workload

import (
	"context"
	"sync"
	"time"
)

// SchedulerConfig parameterises the channel ping-pong primitive.
//
// The primitive launches Pairs goroutine-pairs; each pair pings a token
// back and forth on a pair of unbuffered channels. The aggregate
// channel-op rate across all pairs targets OpsPerSec.
type SchedulerConfig struct {
	Pairs     int
	OpsPerSec int
}

// RunScheduler runs the channel ping-pong primitive until ctx is done.
//
// The token starts in the "ping" channel, ticks pace it across the pair
// at OpsPerSec/(2*Pairs) per direction so the aggregate rate matches
// OpsPerSec. A zero or negative rate means run as fast as possible,
// which is the correct mode for profiles that explicitly want the
// scheduler under load.
func RunScheduler(ctx context.Context, cfg SchedulerConfig) {
	if cfg.Pairs <= 0 {
		return
	}
	var wg sync.WaitGroup
	wg.Add(cfg.Pairs)
	// Per-pair op rate. OpsPerSec counts every send (so a ping+pong
	// equals two ops). Splitting evenly across pairs keeps each
	// goroutine pacing independently and avoids contention on a
	// shared rate-limiter.
	var perPairInterval time.Duration
	if cfg.OpsPerSec > 0 {
		opsPerPair := cfg.OpsPerSec / cfg.Pairs
		if opsPerPair < 1 {
			opsPerPair = 1
		}
		perPairInterval = time.Second / time.Duration(opsPerPair)
	}
	for i := 0; i < cfg.Pairs; i++ {
		go func() {
			defer wg.Done()
			ping := make(chan struct{}, 1)
			pong := make(chan struct{}, 1)
			ping <- struct{}{}
			var ticker *time.Ticker
			var tickC <-chan time.Time
			if perPairInterval > 0 {
				ticker = time.NewTicker(perPairInterval)
				defer ticker.Stop()
				tickC = ticker.C
			}
			for {
				select {
				case <-ctx.Done():
					return
				case v := <-ping:
					if tickC != nil {
						select {
						case <-ctx.Done():
							return
						case <-tickC:
						}
					}
					select {
					case pong <- v:
					case <-ctx.Done():
						return
					}
				case v := <-pong:
					if tickC != nil {
						select {
						case <-ctx.Done():
							return
						case <-tickC:
						}
					}
					select {
					case ping <- v:
					case <-ctx.Done():
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}
