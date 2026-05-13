package workload

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// SyscallTarget selects which kind of blocking syscall the primitive
// produces. Both targets are sized to force runtime M-creation under
// load.
type SyscallTarget string

const (
	SyscallNone        SyscallTarget = "none"
	SyscallLoopbackTCP SyscallTarget = "loopback_tcp"
	SyscallTmpfsFsync  SyscallTarget = "tmpfs_fsync"
)

// SyscallConfig parameterises the syscall primitive.
//
// Goroutines independently issue syscalls at PerGoroutineRate =
// SyscallsPerSec / Goroutines. For loopback_tcp, an in-process echo
// server runs on a free localhost port and Goroutines clients each
// open a long-lived connection and write+read fixed-size payloads. For
// tmpfs_fsync, each goroutine writes+fsyncs a small file under TmpfsDir.
type SyscallConfig struct {
	Target         SyscallTarget
	Goroutines     int
	SyscallsPerSec int
	PayloadBytes   int    // loopback_tcp: bytes per write/read pair; default 256
	TmpfsDir       string // tmpfs_fsync: directory; default /dev/shm
}

// RunSyscall runs the syscall primitive until ctx is done.
//
// On error during setup the function returns early; transient I/O
// errors during the run are logged-and-continue style (silently dropped
// here — the chaos-worker emits its own observability).
func RunSyscall(ctx context.Context, cfg SyscallConfig) error {
	if cfg.Goroutines <= 0 || cfg.SyscallsPerSec <= 0 {
		return nil
	}
	switch cfg.Target {
	case SyscallNone, "":
		return nil
	case SyscallLoopbackTCP:
		return runLoopbackTCP(ctx, cfg)
	case SyscallTmpfsFsync:
		return runTmpfsFsync(ctx, cfg)
	default:
		return fmt.Errorf("unknown syscall target %q", cfg.Target)
	}
}

func runLoopbackTCP(ctx context.Context, cfg SyscallConfig) error {
	if cfg.PayloadBytes <= 0 {
		cfg.PayloadBytes = 256
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("loopback listen: %w", err)
	}
	defer ln.Close()
	addr := ln.Addr().String()

	var serverWG sync.WaitGroup
	serverWG.Add(1)
	go func() {
		defer serverWG.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, cfg.PayloadBytes)
				for {
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
					if _, err := c.Write(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	perGoroutine := cfg.SyscallsPerSec / cfg.Goroutines
	if perGoroutine < 1 {
		perGoroutine = 1
	}
	interval := time.Second / time.Duration(perGoroutine)

	var clientWG sync.WaitGroup
	clientWG.Add(cfg.Goroutines)
	for i := 0; i < cfg.Goroutines; i++ {
		go func() {
			defer clientWG.Done()
			c, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			defer c.Close()
			buf := make([]byte, cfg.PayloadBytes)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := c.Write(buf); err != nil {
						return
					}
					if _, err := io.ReadFull(c, buf); err != nil {
						return
					}
				}
			}
		}()
	}
	<-ctx.Done()
	_ = ln.Close()
	clientWG.Wait()
	serverWG.Wait()
	return nil
}

func runTmpfsFsync(ctx context.Context, cfg SyscallConfig) error {
	dir := cfg.TmpfsDir
	if dir == "" {
		dir = "/dev/shm"
	}
	// Verify dir exists and is writable. On macOS /dev/shm doesn't
	// exist; the harness is RHEL-targeted but builds anywhere, so a
	// fallback to os.TempDir keeps non-RHEL builds workable.
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		dir = os.TempDir()
	}
	perGoroutine := cfg.SyscallsPerSec / cfg.Goroutines
	if perGoroutine < 1 {
		perGoroutine = 1
	}
	interval := time.Second / time.Duration(perGoroutine)
	if cfg.PayloadBytes <= 0 {
		cfg.PayloadBytes = 256
	}

	var wg sync.WaitGroup
	wg.Add(cfg.Goroutines)
	for i := 0; i < cfg.Goroutines; i++ {
		path := filepath.Join(dir, fmt.Sprintf("chaos-fsync-%d-%d", os.Getpid(), i))
		go func(path string) {
			defer wg.Done()
			f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
			if err != nil {
				return
			}
			defer func() {
				_ = f.Close()
				_ = os.Remove(path)
			}()
			payload := make([]byte, cfg.PayloadBytes)
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := f.WriteAt(payload, 0); err != nil {
						return
					}
					if err := f.Sync(); err != nil {
						return
					}
				}
			}
		}(path)
	}
	wg.Wait()
	return nil
}
