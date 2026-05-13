// Package output provides a JSONL event emitter used by chaos-worker.
//
// Events are buffered in a small channel and written by a single
// goroutine to keep the file write off the hot path. Emit is non-
// blocking: if the buffer fills the event is dropped and an internal
// counter increments (reported on Close). Dropping is preferable to
// blocking the worker — the harness measures its own observability
// overhead in the next-shot validation phase.
package output

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

// Writer streams JSONL events to a file.
type Writer struct {
	f       *os.File
	w       *bufio.Writer
	ch      chan map[string]any
	dropped atomic.Uint64
	wg      sync.WaitGroup
	closed  atomic.Bool
}

// NewWriter opens path for append-create and starts the flusher goroutine.
//
// Buffer is the channel capacity for buffered events; values around
// 4096 are reasonable. A smaller buffer increases the chance of drops
// under bursty emission.
func NewWriter(path string, buffer int) (*Writer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	w := &Writer{
		f:  f,
		w:  bufio.NewWriterSize(f, 64*1024),
		ch: make(chan map[string]any, buffer),
	}
	w.wg.Add(1)
	go w.run()
	return w, nil
}

// Emit queues an event. Non-blocking; on overflow the event is dropped.
func (w *Writer) Emit(event map[string]any) {
	if w.closed.Load() {
		return
	}
	select {
	case w.ch <- event:
	default:
		w.dropped.Add(1)
	}
}

// Close flushes and closes the writer. Safe to call once. Returns the
// dropped-event count so the caller can include it in a final event.
func (w *Writer) Close() (uint64, error) {
	if !w.closed.CompareAndSwap(false, true) {
		return w.dropped.Load(), nil
	}
	close(w.ch)
	w.wg.Wait()
	if err := w.w.Flush(); err != nil {
		_ = w.f.Close()
		return w.dropped.Load(), err
	}
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return w.dropped.Load(), err
	}
	return w.dropped.Load(), w.f.Close()
}

func (w *Writer) run() {
	defer w.wg.Done()
	enc := json.NewEncoder(w.w)
	enc.SetEscapeHTML(false)
	for ev := range w.ch {
		_ = enc.Encode(ev)
	}
}
