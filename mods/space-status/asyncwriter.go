package main

import (
	"os"
	"sync"
)

// asyncWriter buffers the latest data in memory and flushes to disk
// in a background goroutine. Callers never block on I/O.
type asyncWriter struct {
	path    string
	mu      sync.Mutex
	pending []byte // nil = nothing to flush
	prev    []byte // last successfully written data (for dedup)
	wake    chan struct{}
	done    chan struct{}
}

func newAsyncWriter(path string) *asyncWriter {
	w := &asyncWriter{
		path: path,
		wake: make(chan struct{}, 1),
		done: make(chan struct{}),
	}
	// Seed prev with current file content for dedup.
	if data, err := os.ReadFile(path); err == nil {
		w.prev = data
	}
	go w.loop()
	return w
}

// Write stores data in memory and signals the background flusher.
// Returns immediately without touching disk.
func (w *asyncWriter) Write(data []byte) {
	w.mu.Lock()
	// Skip if identical to what's already on disk.
	if bytesEqual(data, w.prev) {
		w.mu.Unlock()
		return
	}
	w.pending = make([]byte, len(data))
	copy(w.pending, data)
	w.mu.Unlock()
	// Non-blocking signal.
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Stop shuts down the background goroutine and does a final flush.
func (w *asyncWriter) Stop() {
	close(w.wake)
	<-w.done
}

func (w *asyncWriter) loop() {
	defer close(w.done)
	for range w.wake {
		w.flush()
	}
	// Final flush after channel closes.
	w.flush()
}

func (w *asyncWriter) flush() {
	w.mu.Lock()
	data := w.pending
	w.pending = nil
	w.mu.Unlock()
	if data == nil {
		return
	}
	if err := atomicWrite(w.path, data); err != nil {
		return
	}
	w.mu.Lock()
	w.prev = data
	w.mu.Unlock()
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
