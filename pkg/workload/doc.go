// Package workload provides primitive workload generators that the
// chaos-worker composes into a profile.
//
// The four primitives are:
//
//   - scheduler: channel ping-pong between goroutine pairs. Drives Go
//     scheduler pressure and exposes work-stealing / runqueue behaviour.
//   - alloc: heap allocation at a target byte-rate with a tunable size
//     distribution. Drives GC pressure.
//   - syscall: blocking syscalls (loopback TCP echo or tmpfs fsync) that
//     force the runtime to create new OS threads (M's) when goroutines
//     block.
//   - lock: contended mutex acquisitions that produce wakeups and
//     futex churn.
//
// Each primitive exposes a Run(ctx, config) function that runs until
// the context is cancelled. Run is goroutine-safe and may be called
// from multiple worker goroutines.
package workload
